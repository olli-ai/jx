package kube

import (
	"fmt"
	"os"
	"strconv"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func (o *StepHelmApplyOptions) acquireLock(kubeClient kubernetes.Interface) (func() error, error) {
	// Get infos from the headers
	owner := os.Getenv("REPO_OWNER")
	repository := os.Getenv("REPO_NAME")
	branch := os.Getenv("BRANCH_NAME")
	build := os.Getenv("BUILD_NUMBER")
	nbuild, err := strconv.Atoi(build)
	if err != nil {
		log.Logger().Warnf("cannot parse build number %s: %s\n", build, err.Error())
		return nil, err
	}
	// Find our pod
	podList, err := kubeClient.CoreV1().Pods(o.devNamespace).List(metav1.ListOptions{
		LabelSelector: fmt.Printf("owner=%s,repository=%s,branch=%s,build=%s,jenkins.io/pipelineType=build", owner, repository, branch, build),
	})
	if err != nil {
		return nil, err
	} else if len(podList) != 1 {
		return nil, fmt.Errorf("%d pods found for this job", len(podList))
	}
	pod := podList[0]
	// Create the lock object
	lock := &v1.ConfigMap {
		metav1.ObjectMeta {
			// TODO lock at namespace level
			Name:            fmt.Printf("jx-lock-%s-%s-%s", owner, repository, branch),
			Namespace:       o.devNamespace,
			Labels:          map[string]string {
				"jenkins-x.io/created-by": "Jenkins X",
				"jenkins-x.io/kind":       "lock",
				"owner":      owner,
				"repository": repository,
				"branch":     branch,
				"build":      build,
			},
			OwnerReferences: []metav1.OwnerReference{
				APIVersion: pod.APIVersion,
				Kind:       pod.Kind,
				Name:       pod.Name,
				UID:        pod.UID,
			},
		},
		Data: map[string]string {
			"build":   build,
			"waiting": build,
			"pod":     pod.Name,
		},
	}
	// this loop continuously tries to create the lock
	Create: for {
		log.Logger().Infof("creating the lock configmap %s", lock.Name)
		// create the lock
		new, err := kubeClient.CoreV1().ConfigMaps(o.devNamespace).Create(lock)
		if err != nil {
			status, ok := err.(*errors.StatusError)
			// an error while creating the lock
			if !ok || status.Status().Reason != metav1.StatusReasonAlreadyExists {
				log.Logger().Warnf("failed to create the lock configmap %s: %s\n", lock.Name, err.Error())
				return nil, err
			}
			// there is already a similat lock
			log.Logger().Infof("lock configmap %s already exists", lock.Name)
		} else {
			// the lock is created, can now perform the updates
			log.Logger().Infof("lock configmap %s created", lock.Name)
			return func() {
				log.Logger().Infof("cleaning the lock configmap %s", lock.Name)
				err := kubeClient.CoreV1().ConfigMaps(o.devNamespace).Delete(lock.Name,
					&metav1.DeleteOptions {
						&Preconditions {
							UID: new.UID
						}
					})
				if err != nil {
					log.Logger().Warnf("failed to cleanup the lock configmap %s: %s\n", lock.Name, err.Error())
				}
			}, nil
		}
		// create these variables outside, to be able to edit them before the next loop
		var old *v1.ConfigMap
		var pod *v1.ConfigMap
		Read: for {
			// get the current lock if not already provided
			if old == nil {
				old, err = kubeClient.CoreV1().ConfigMaps(o.devNamespace).Get(lock.Name)
				if err != nil {
					status, ok := err.(*errors.StatusError)
					// the lock does not exist anymore, try to create it
					if ok && status.Status().Reason == metav1.StatusReasonNotFound {
						log.Logger().Infof("lock configmap %s deleted", lock.Name)
						continue Create
					}
					// an error getting the lock
					log.Logger().Warnf("failed to get the lock configmap %s: %s\n", lock.Name, err.Error())
					return nil, err
				}
			}
			// parse the waiting argument
			if nwaiting, err := strconv.Atoi(old.Data["waiting"]); err != nil {
				log.Logger().Warnf("cannot parse waiting build number %s: %s\n", old.Data["waiting"], err.Error())
				return nil, err
			// the waiting build is higher than our own build, should stop
			} else if nwaiting > nbuild {
				log.Logger().Warnf("the later build %d is already waiting", nwaiting)
				return nil, fmt.Errorf("the later build %d is already waiting", nwaiting)
			// the waiting build is lower than our own, change it by out own
			} else if nwaiting < nbuild {
				old.Data["waiting"] = build
				old, err = kubeClient.CoreV1().ConfigMaps(o.devNamespace).Update(old)
				if err != nil {
					status, ok := err.(*errors.StatusError)
					// the lock does not exist anymore, try to create it
					if ok && status.Status().Reason == metav1.StatusReasonNotFound {
						log.Logger().Infof("lock configmap %s deleted", lock.Name)
						continue Create
					// the lock has changed, read it again
					} else if ok && status.Status().Reason == metav1.StatusReasonConflict {
						log.Logger().Infof("lock configmap %s changed", lock.Name)
						old = nil
						continue Read
					}
					// an error updating the lock
					log.Logger().Warnf("failed to update the lock configmap %s: %s\n", lock.Name, err.Error())
					return nil, err
				}
			}
			remove := false // if the lock should simply be removed
			// get the current locking pod if not already provided
			if pod == nil || pod.Name != old.Data["pod"] {
				pod, err = kubeClient.CoreV1().Pods(o.devNamespace).Get(old.Data["pod"], metav1.GetOptions{})
				if err != nil {
					status, ok := err.(*errors.StatusError)
					// the pod does not exist anymore, the lock should be removed
					if ok && status.Status().Reason == metav1.StatusReasonNotFound {
						log.Logger().Infof("locking pod %s finished", pod.Name)
						remove = true
					// an error while getting the pod
					} else {
						log.Logger().Warnf("failed to get the locking pod %s: %s\n", old.Data["pod"], err.Error())
						return nil, err
					}
				}
			}
			// check the pod's phase
			if pod != nil {
				log.Logger().Infof("locking pod %s is in phase %s", pod.Name, pod.Phase)
				remove = pod.Phase != "Pending" && pod.Phase != "Running"
			}
			// remove the lock
			if remove {
				log.Logger().Infof("cleaning the old lock configmap %s", lock.Name)
				err := kubeClient.CoreV1().ConfigMaps(o.devNamespace).Delete(lock.Name,
					&metav1.DeleteOptions {
						&Preconditions {
							UID: old.UID
						}
					})
				// removed, now try to create it
				if err == nil {
					continue Create
				}
				status, ok := err.(*errors.StatusError)
				// already deleted, try to create it
				if ok && status.Status().Reason == metav1.StatusReasonNotFound {
					continue Create
				// the lock changed, read it again
				} else if ok && status.Status().Reason == metav1.StatusReasonConflict {
					log.Logger().Infof("lock configmap %s changed", lock.Name)
					lock = nil
					continue Read
				// an error while removing the pod
				} else {
					log.Logger().Warnf("failed to cleanup the old lock configmap %s: %s\n", lock.Name, err.Error())
					return nil, err
				}
			}
			// watch both the pod and the lock for updates
			log.Logger().Infof("waiting for updates on the lock configmap %s", lock.Name)
			lockWatch, err := kubeClient.CoreV1().ConfigMaps(o.devNamespace).Watch(metav1.SingleObject(old))
			if err != nil {
				log.Logger().Warnf("cannot watch the lock configmap %s: %s\n", lock.Name, err.Error())
				return nil, err
			}
			podWatch, err := kubeClient.CoreV1().ConfigMaps(o.devNamespace).Watch(metav1.SingleObject(pod))
			if err != nil {
				log.Logger().Warnf("cannot watch the locking pod %s: %s\n", pod.Name, err.Error())
				return nil, err
			}
			lockChan := lockWatch.ResultChan()
			podChan := lockWatch.ResultChan()
			cleanup := func() {
				lockWatch.Stop()
				podWatch.Stop()
			}
			for {
				select {
				// an event about the lock
				case event := <- lockChan:
					switch event.Type {
					// the lock has changed
					case watch.Added, watch.Modified:
						old = event.Object.(*v1.ConfigMap)
						// if the waiting build has changed, read again
						if nwaiting, err := strconv.Atoi(old.Data["waiting"]); err != nil {
							log.Logger().Warnf("cannot parse waiting build number %s: %s\n", old.Data["waiting"], err.Error())
							return nil, err
						} else if nwaiting != nbuild {
							cleanup()
							continue Read
						}
					// the lock is deleted, try to create it
					case watch.Deleted:
						cleanup()
						continue Create
					// an error
					case watch.Error:
						err := errors.FromObject(event.Object)
						log.Logger().Warnf("cannot watch the lock configmap %s: %s\n", lock.Name, err.Error())
						return nil, err
					}
				case event := <- podChan:
					switch event.Type {
					// the pod has changed, if its phase has changed,
					// let's assume that the configmap has been deleted
					case watch.Added, watch.Modified:
						pod = event.Object.(*v1.Pod)
						if pod.Phase != "Pending" && pod.Phase != "Running" {
							cleanup()
							continue Create
						}
					}
					// the pod was deleted, let's assume the configmap too
					case watch.Deleted:
						cleanup()
						continue Create
					// an error
					case watch.Error:
						err := errors.FromObject(event.Object)
						log.Logger().Warnf("cannot watch the locking pod %s: %s\n", pod.Name, err.Error())
						return nil, err
					}
				}
			}
		}
	}
}

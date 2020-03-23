package kube

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jenkins-x/jx/pkg/log"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// Labels required to be a lock. Anything else should be ignored
var buildLockLabels map[string]string = map[string]string {
	"jenkins-x.io/kind": "build-lock",
}

// Acquires a build lock, to avoid other builds to edit the same namespace
// while a deployment is already running, other deployment can negotiate which one
// should run after, by editing its data.
// Returns a function to release the lock (to be called in a defer)
// Returns an error if a newer build is already running, or if an error happened
func AcquireBuildLock(kubeClient kubernetes.Interface, devNamespace, namespace string) (func() error, error) {
	// Get infos from the headers
	now := time.Now().UTC().Format(time.RFC3339Nano)
	owner := os.Getenv("REPO_OWNER")
	if owner == "" {
		log.Logger().Warnf("no REPO_OWNER provided")
		return nil, fmt.Errorf("no REPO_OWNER provided")
	}
	repository := os.Getenv("REPO_NAME")
	if repository == "" {
		log.Logger().Warnf("no REPO_NAME provided")
		return nil, fmt.Errorf("no REPO_NAME provided")
	}
	branch := os.Getenv("BRANCH_NAME")
	if branch == "" {
		log.Logger().Warnf("no BRANCH_NAME provided")
		return nil, fmt.Errorf("no BRANCH_NAME provided")
	}
	build := os.Getenv("BUILD_NUMBER")
	if _, err := strconv.Atoi(build); err != nil {
		log.Logger().Warnf("no BUILD_NUMBER provided: %s\n", build, err.Error())
		return nil, err
	}
	// Find our pod
	podList, err := kubeClient.CoreV1().Pods(devNamespace).List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("owner=%s,repository=%s,branch=%s,build=%s,jenkins.io/pipelineType=build", owner, repository, branch, build),
	})
	if err != nil {
		return nil, err
	} else if len(podList.Items) != 1 {
		return nil, fmt.Errorf("%d pods found for this job (owner=%s,repository=%s,branch=%s,build=%s,jenkins.io/pipelineType=build)",
			len(podList.Items), owner, repository, branch, build)
	}
	pod := &podList.Items[0]
	// kubernetes library seems to forget APIVersoin and Kind
	// fill those if they're missing
	if pod.APIVersion == "" {
		pod.APIVersion = "v1"
	}
	if pod.Kind == "" {
		pod.Kind = "Pod"
	}
	podKind := pod.Kind
	// Create the lock object
	lock := &v1.ConfigMap {
		ObjectMeta: metav1.ObjectMeta {
			Name:            fmt.Sprintf("jx-lock-%s", namespace),
			Namespace:       devNamespace,
			Labels:          map[string]string {
				"namespace":  namespace,
				"owner":      owner,
				"repository": repository,
				"branch":     branch,
				"build":      build,
			},
			Annotations:     map[string]string {
				"jenkins-x.io/created-by": "Jenkins X",
				"warning": "DO NOT REMOVE",
				"purpose": fmt.Sprintf("This is a deployment lock for the " +
					"namespace \"%s\". It prevents several deployments to " +
					"edit the same namespace at the same time. It will " +
					"automatically be removed once the deployemnt is " +
					"finished, or replaced by the next deployemnt to run.",
					namespace),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: pod.APIVersion,
				Kind:       pod.Kind,
				Name:       pod.Name,
				UID:        pod.UID,
			}},
		},
		Data:       map[string]string {
			"namespace":  namespace,
			"owner":      owner,
			"repository": repository,
			"branch":     branch,
			"build":      build,
			"pod":        pod.Name,
			"timestamp":  now,
		},
	}
	for k, v := range buildLockLabels {
		lock.Labels[k] = v
	}
	// this loop continuously tries to create the lock
	Create: for {
		log.Logger().Infof("creating the lock configmap %s", lock.Name)
		// create the lock
		new, err := kubeClient.CoreV1().ConfigMaps(devNamespace).Create(lock)
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
			// returns a function that releases the lock
			return func() error {
				log.Logger().Infof("cleaning the lock configmap %s", lock.Name)
				err := kubeClient.CoreV1().ConfigMaps(devNamespace).Delete(lock.Name,
					&metav1.DeleteOptions {
						Preconditions: &metav1.Preconditions {
							UID: &new.UID,
						},
					})
				if err != nil {
					log.Logger().Warnf("failed to cleanup the lock configmap %s: %s\n", lock.Name, err.Error())
				}
				return err
			}, nil
		}
		// create these variables outside, to be able to edit them before the next loop
		var old *v1.ConfigMap
		var pod *v1.Pod
		Read: for {
			// get the current lock if not already provided
			if old == nil {
				old, err = kubeClient.CoreV1().ConfigMaps(devNamespace).Get(lock.Name, metav1.GetOptions{})
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
			// check if the lock should not simply be removed
			remove := false
			// check the lock
			for k, v := range buildLockLabels {
				if old.Labels[k] != v {
					log.Logger().Warnf("the lock %s should have annotation \"%s: %s\"", old.Name, k, v)
					remove = true
				}
			}
			if !remove && old.Labels["namespace"] != namespace {
				log.Logger().Warnf("the lock %s should have label \"namespace: %s\"", old.Name, namespace)
				remove = true
			}
			var owner *metav1.OwnerReference
			if !remove {
				if len(old.OwnerReferences) != 1 {
					log.Logger().Warnf("the lock %s has %d OwnerReferences", old.Name, len(old.OwnerReferences))
					remove = true
				} else if owner = &old.OwnerReferences[0]; owner.Kind != podKind || owner.Name == "" || owner.UID == "" {
					log.Logger().Warnf("the lock %s has invalid OwnerReference %v", old.Name, owner)
					remove = true
				}
			}
			// get the current locking pod if not already provided
			if !remove && (pod == nil || pod.Name != owner.Name || pod.UID != owner.UID) {
				pod, err = kubeClient.CoreV1().Pods(devNamespace).Get(owner.Name, metav1.GetOptions{})
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
				} else if pod.UID != owner.UID {
					log.Logger().Infof("locking pod %s finished", pod.Name)
					remove = true
				}
			}
			// check the pod's phase
			if !remove && pod != nil {
				log.Logger().Infof("locking pod %s is in phase %s", pod.Name, pod.Status.Phase)
				remove = pod.Status.Phase != "Pending" && pod.Status.Phase != "Running"
			}
			// remove the lock
			if remove {
				log.Logger().Infof("cleaning the old lock configmap %s", lock.Name)
				err := kubeClient.CoreV1().ConfigMaps(devNamespace).Delete(lock.Name,
					&metav1.DeleteOptions {
						Preconditions: &metav1.Preconditions {
							UID: &old.UID,
						},
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
					old = nil
					continue Read
				// an error while removing the pod
				} else {
					log.Logger().Warnf("failed to cleanup the old lock configmap %s: %s\n", lock.Name, err.Error())
					return nil, err
				}
			}
			// compare the builds
			if data, err := compareBuildLocks(old.Data, lock.Data); err != nil {
				return nil, err
			// should update the build to wait
			} else if data != nil {
				old.Data = data
				old, err = kubeClient.CoreV1().ConfigMaps(devNamespace).Update(old)
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
			// watch the lock for updates
			if old, err = watchBuildLock(kubeClient, old, pod, lock.Data); err != nil {
				return nil, err
			// lock configmap was updated, read it again
			} else if old != nil {
				continue Read
			// lock configmap was (probably) deleted, try to create it again
			} else {
				continue Create
			}
		}
	}
}

func watchBuildLock(kubeClient kubernetes.Interface, lock *v1.ConfigMap, pod *v1.Pod, build map[string]string) (*v1.ConfigMap, error) {
	// watch both the pod and the lock for updates
	log.Logger().Infof("waiting for updates on the lock configmap %s", lock.Name)
	lockWatch, err := kubeClient.CoreV1().ConfigMaps(lock.Namespace).Watch(metav1.SingleObject(lock.ObjectMeta))
	if err != nil {
		log.Logger().Warnf("cannot watch the lock configmap %s: %s\n", lock.Name, err.Error())
		return nil, err
	}
	defer lockWatch.Stop()
	podWatch, err := kubeClient.CoreV1().ConfigMaps(pod.Namespace).Watch(metav1.SingleObject(pod.ObjectMeta))
	if err != nil {
		log.Logger().Warnf("cannot watch the locking pod %s: %s\n", pod.Name, err.Error())
		return nil, err
	}
	defer podWatch.Stop()
	lockChan := lockWatch.ResultChan()
	podChan := lockWatch.ResultChan()
	for {
		select {
		// an event about the lock
		case event := <- lockChan:
			switch event.Type {
			// the lock has changed
			case watch.Added, watch.Modified:
				lock := event.Object.(*v1.ConfigMap)
				// if the waiting build has changed, read again
				if next, err := compareBuildLocks(lock.Data, build); err != nil {
					return nil, err
				} else if next != nil {
					return lock, nil
				}
			// the lock is deleted, try to create it
			case watch.Deleted:
				return nil, nil
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
				pod := event.Object.(*v1.Pod)
				if pod.Status.Phase != "Pending" && pod.Status.Phase != "Running" {
					return nil, nil
				}
			// the pod was deleted, let's assume the configmap too
			case watch.Deleted:
				return nil, nil
			// an error
			case watch.Error:
				err := errors.FromObject(event.Object)
				log.Logger().Warnf("cannot watch the locking pod %s: %s\n", pod.Name, err.Error())
				return nil, err
			}
		}
	}
}

// Campares two builds
// If next is nil, the build is already waiting
// if next is not nil, the build should wait, and save these data
func compareBuildLocks(old, new map[string]string) (next map[string]string, err error) {
	sameRepo := true
	for _, k := range [3]string{"owner", "repository", "branch"} {
		if old[k] != new[k] {
			sameRepo = false
		}
	}
	// both are deplying the same repo and branch, compare build number
	if sameRepo {
		// same build and pod, we're already waiting
		if old["build"] == new["build"] {
			return nil, nil
		}
		// parse the builds
		if oldBuild, err := strconv.Atoi(old["build"]); err != nil {
			log.Logger().Warnf("cannot parse the lock's build number %s: %s\n", old["build"], err.Error())
			return nil, err
		} else if newBuild, err := strconv.Atoi(new["build"]); err != nil {
			log.Logger().Warnf("cannot parse the lock's build number %s: %s\n", new["build"], err.Error())
			return nil, err
		// older build, give up
		} else if oldBuild >= newBuild {
			log.Logger().Warnf("newer build %d is waiting already", oldBuild)
			return nil, fmt.Errorf("newer build %d is waiting already", oldBuild)
		}
		// parse the timestamps in order to keep th newest one
		if oldTime, err := time.Parse(time.RFC3339Nano, old["timestamp"]); err != nil {
			log.Logger().Warnf("cannot parse the lock's timestamp %s: %s\n", old["timestamp"], err.Error())
			return nil, err
		} else if newTime, err := time.Parse(time.RFC3339Nano, new["timestamp"]); err != nil {
			log.Logger().Warnf("cannot parse the lock's timestamp %s: %s\n", new["timestamp"], err.Error())
			return nil, err
		// keep increasing the timestamp, for consistency reasons
		} else if oldTime.After(newTime) {
			next := map[string]string {}
			for k, v := range(new) {
				next[k] = v
			}
			next["timestamp"] = old["timestamp"]
			return next, nil
		// timestamp already right
		} else {
			return new, nil
		}
	// both are deploying different repos, keep the newest one
	// it is a corner case for consistency
	// but should not happen on a standard cluster
	} else {
		// parse the timestamps
		if oldTime, err := time.Parse(time.RFC3339Nano, old["timestamp"]); err != nil {
			log.Logger().Warnf("cannot parse the lock's timestamp %s: %s\n", old["timestamp"], err.Error())
			return nil, err
		} else if newTime, err := time.Parse(time.RFC3339Nano, new["timestamp"]); err != nil {
			log.Logger().Warnf("cannot parse the lock's timestamp %s: %s\n", new["timestamp"], err.Error())
			return nil, err
		// newer deployment, wait
		} else if newTime.After(oldTime) {
			return new, nil
		// older deployment, give up
		} else {
			return nil, fmt.Errorf("newer build %s is waiting already", oldTime)
		}
	}
}

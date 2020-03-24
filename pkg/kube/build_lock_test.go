package kube

import (
	"fmt"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_compareBuildLocks(t *testing.T) {
	time1 := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	time2 := time.Date(2000, 1, 1, 0, 0, 0, 200000000, time.UTC).Format(time.RFC3339Nano)
	time3 := time.Date(2000, 1, 1, 0, 0, 0, 210000000, time.UTC).Format(time.RFC3339Nano)
	examples := []struct{
		name string
		old  map[string]string
		new  map[string]string
		ret  map[string]string
		err  bool
	}{{
		"same build",
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "123",
			"pod":        "build-pod-123",
			"timestamp":  time2,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "123",
			"pod":        "build-pod-123",
			"timestamp":  time2,
		},
		nil,
		false,
	},{
		"same build but different pod (for some reason)",
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "123",
			"pod":        "build-pod-123",
			"timestamp":  time2,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "123",
			"pod":        "other-pod-123",
			"timestamp":  time2,
		},
		nil,
		true,
	},{
		"lower build",
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "101",
			"pod":        "build-pod-101",
			"timestamp":  time2,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "99",
			"pod":        "build-pod-99",
			"timestamp":  time1,
		},
		nil,
		true,
	},{
		"higher build",
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "101",
			"pod":        "build-pod-101",
			"timestamp":  time1,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "103",
			"pod":        "build-pod-103",
			"timestamp":  time2,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "103",
			"pod":        "build-pod-103",
			"timestamp":  time2,
		},
		false,
	},{
		"higher build but lower timestamp",
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "101",
			"pod":        "build-pod-101",
			"timestamp":  time3,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "103",
			"pod":        "build-pod-103",
			"timestamp":  time2,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "103",
			"pod":        "build-pod-103",
			"timestamp":  time3,
		},
		false,
	},{
		"other build, same timestamp",
		map[string]string{
			"owner":      "other-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "111",
			"pod":        "build-pod-111",
			"timestamp":  time2,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "123",
			"pod":        "other-pod-123",
			"timestamp":  time2,
		},
		nil,
		true,
	},{
		"other build, lower timestamp",
		map[string]string{
			"owner":      "my-owner",
			"repository": "other-repository",
			"branch":     "my-branch",
			"build":      "111",
			"pod":        "build-pod-111",
			"timestamp":  time2,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "123",
			"pod":        "other-pod-123",
			"timestamp":  time1,
		},
		nil,
		true,
	},{
		"other build, higher timestamp",
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "other-branch",
			"build":      "111",
			"pod":        "build-pod-111",
			"timestamp":  time2,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "123",
			"pod":        "other-pod-123",
			"timestamp":  time3,
		},
		map[string]string{
			"owner":      "my-owner",
			"repository": "my-repository",
			"branch":     "my-branch",
			"build":      "123",
			"pod":        "other-pod-123",
			"timestamp":  time3,
		},
		false,
	}}
	for _, example := range examples {
		ret, err := compareBuildLocks(example.old, example.new)
		assert.Equal(t, example.ret, ret, example.name)
		if example.err {
			assert.Error(t, err, example.name)
		} else {
			assert.NoError(t, err, example.name)
		}
	}
}

// Creates a fake client with a fake tekton deployment
func build_lock_client(t *testing.T) *fake.Clientset {
	client := fake.NewSimpleClientset()
	_, err := client.AppsV1().Deployments("jx").Create(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentTektonController,
			Namespace: "jx",
		},
	})
	require.NoError(t, err)
	return client
}

var build_lock_uid int = 1 << 20
// Creates a running pod, looking close enough to a pipeline pod
func build_lock_pod(t *testing.T, client kubernetes.Interface, owner, repository, branch, build string) *v1.Pod {
	build_lock_uid ++
	pod, err := client.CoreV1().Pods("jx").Create(&v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("pipeline-%s-%s-%s-%s", owner, repository, branch, build),
			Namespace: "jx",
			Labels: map[string]string{
				"owner":                   owner,
				"repository":              repository,
				"branch":                  branch,
				"build":                   build,
				"jenkins.io/pipelineType": "build",
			},
			UID:        types.UID(fmt.Sprintf("%d", build_lock_uid)),
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	})
	require.NoError(t, err)
	return pod
}

func build_lock_lock(t *testing.T, client kubernetes.Interface, namespace string, pod *v1.Pod, minutes int) *v1.ConfigMap {
	lock, err := client.CoreV1().ConfigMaps("jx").Create(&v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "jx-lock-my-namespace",
			Namespace: "jx",
			Labels: map[string]string{
				"namespace":         namespace,
				"owner":             pod.Labels["owner"],
				"repository":        pod.Labels["repository"],
				"branch":            pod.Labels["branch"],
				"build":             pod.Labels["build"],
				"jenkins-x.io/kind": "build-lock",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       pod.Name,
				UID:        pod.UID,
			}},
		},
		Data: map[string]string{
			"namespace":  namespace,
			"owner":      pod.Labels["owner"],
			"repository": pod.Labels["repository"],
			"branch":     pod.Labels["branch"],
			"build":      pod.Labels["build"],
			"pod":        pod.Name,
			"timestamp":  build_lock_timestamp(minutes),
		},
	})
	require.NoError(t, err)
	return lock
}

func build_lock_timestamp(minutes int) string {
	now := time.Now().UTC()
	now = now.Add(time.Duration(minutes) * time.Minute)
	return now.Format(time.RFC3339Nano)
}

func build_lock_env(t *testing.T, owner, repository, branch, build string) func() {
	env := map[string]string {
		"REPO_OWNER":   owner,
		"REPO_NAME":    repository,
		"BRANCH_NAME":  branch,
		"BUILD_NUMBER": build,
	}
	old := map[string]string {}
	for k, v := range env {
		old[k] = os.Getenv(k)
		err := os.Setenv(k, v)
		require.NoError(t, err)
	}
	return func() {
		for k, v := range old {
			err := os.Setenv(k, v)
			assert.NoError(t, err)
		}
	}
}

func build_lock_acquire(t *testing.T, client kubernetes.Interface, namespace string, pod *v1.Pod, fails bool) chan func() error {
	c := make(chan func() error)
	go func() {
		clean := build_lock_env(t, pod.Labels["owner"], pod.Labels["repository"], pod.Labels["branch"], pod.Labels["build"])
		defer clean()
		callback, err := AcquireBuildLock(client, "jx", namespace)
		if !fails {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
		c <- callback
	}()
	return c
}

func checkConfigmap(t *testing.T, client kubernetes.Interface, namespace string, pod *v1.Pod) {
	lock, err := client.CoreV1().ConfigMaps("jx").Get("jx-lock-" + namespace, metav1.GetOptions{})
	if pod == nil {
		assert.Nil(t, lock)
		if assert.Error(t, err) {
			require.IsType(t, &errors.StatusError{}, err)
			status := err.(*errors.StatusError)
			require.Equal(t, metav1.StatusReasonNotFound, status.Status().Reason)
		}
	} else {
		require.NoError(t, err)
		if assert.NotNil(t, lock) {
			assert.Equal(t, "build-lock", lock.Labels["jenkins-x.io/kind"])
			assert.Equal(t, namespace, lock.Labels["namespace"])
			assert.Equal(t, pod.Labels["owner"], lock.Labels["owner"])
			assert.Equal(t, pod.Labels["repository"], lock.Labels["repository"])
			assert.Equal(t, pod.Labels["branch"], lock.Labels["branch"])
			assert.Equal(t, pod.Labels["build"], lock.Labels["build"])
			assert.Equal(t, []metav1.OwnerReference{{
				APIVersion: pod.APIVersion,
				Kind:       pod.Kind,
				Name:       pod.Name,
				UID:        pod.UID,
			}}, lock.OwnerReferences)
			assert.Equal(t, namespace, lock.Data["namespace"])
			assert.Equal(t, pod.Labels["owner"], lock.Data["owner"])
			assert.Equal(t, pod.Labels["repository"], lock.Data["repository"])
			assert.Equal(t, pod.Labels["branch"], lock.Data["branch"])
			assert.Equal(t, pod.Labels["build"], lock.Data["build"])
			assert.Equal(t, pod.Name, lock.Data["pod"])
			_, err := time.Parse(time.RFC3339Nano, lock.Data["timestamp"])
			assert.NoError(t, err)
		}
	}
}

func TestAcquireBuildLock(t *testing.T) {
	client := build_lock_client(t)
	pod := build_lock_pod(t, client, "my-owner", "my-repository", "my-branch", "13")
	callback := <- build_lock_acquire(t, client, "my-namespace", pod, false)
	checkConfigmap(t, client, "my-namespace", pod)
	require.NoError(t, callback())
	checkConfigmap(t, client, "my-namespace", nil)
}

func TestAcquireBuildLock_previousNotFound(t *testing.T) {
	client := build_lock_client(t)
	previous := build_lock_pod(t, client, "my-owner", "my-repository", "my-branch", "42")
	build_lock_lock(t, client, "my-namespace", previous, 42)
	err := client.CoreV1().Pods("jx").Delete(previous.Name, &metav1.DeleteOptions{})
	require.NoError(t, err)

	pod := build_lock_pod(t, client, "my-owner", "my-repository", "my-branch", "13")
	callback := <- build_lock_acquire(t, client, "my-namespace", pod, false)
	checkConfigmap(t, client, "my-namespace", pod)
	require.NoError(t, callback())
	checkConfigmap(t, client, "my-namespace", nil)
}

func TestAcquireBuildLock_previousFinished(t *testing.T) {
	client := build_lock_client(t)
	previous := build_lock_pod(t, client, "my-owner", "my-repository", "my-branch", "42")
	build_lock_lock(t, client, "my-namespace", previous, 42)
	previous.Status.Phase = v1.PodFailed
	_, err := client.CoreV1().Pods("jx").Update(previous)
	require.NoError(t, err)

	pod := build_lock_pod(t, client, "my-owner", "my-repository", "my-branch", "13")
	callback := <- build_lock_acquire(t, client, "my-namespace", pod, false)
	checkConfigmap(t, client, "my-namespace", pod)
	require.NoError(t, callback())
	checkConfigmap(t, client, "my-namespace", nil)
}

func TestAcquireBuildLock_higherRuns(t *testing.T) {
	client := build_lock_client(t)
	previous := build_lock_pod(t, client, "my-owner", "my-repository", "my-branch", "42")
	build_lock_lock(t, client, "my-namespace", previous, -42)

	pod := build_lock_pod(t, client, "my-owner", "my-repository", "my-branch", "13")
	<- build_lock_acquire(t, client, "my-namespace", pod, true)
}

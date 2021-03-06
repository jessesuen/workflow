package controller

import (
	"encoding/json"
	"fmt"

	wfv1 "github.com/argoproj/argo/api/workflow/v1"
	"github.com/argoproj/argo/errors"
	"github.com/argoproj/argo/workflow/common"
	apiv1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Reusable k8s pod spec portions used in workflow pods
var (
	// volumePodMetadata makes available the pod metadata available as a file
	// to the argoexec init and sidekick containers. Specifically, the template
	// of the pod is stored as an annotation
	volumePodMetadata = apiv1.Volume{
		Name: common.PodMetadataVolumeName,
		VolumeSource: apiv1.VolumeSource{
			DownwardAPI: &apiv1.DownwardAPIVolumeSource{
				Items: []apiv1.DownwardAPIVolumeFile{
					apiv1.DownwardAPIVolumeFile{
						Path: common.PodMetadataAnnotationsVolumePath,
						FieldRef: &apiv1.ObjectFieldSelector{
							APIVersion: "v1",
							FieldPath:  "metadata.annotations",
						},
					},
				},
			},
		},
	}
	volumeMountPodMetadata = apiv1.VolumeMount{
		Name:      volumePodMetadata.Name,
		MountPath: common.PodMetadataMountPath,
	}
	// volumeDockerLib provides the argoexec sidekick container access to the minion's
	// docker containers runtime files (e.g. /var/lib/docker/container). This is required
	// for argoexec to access the main container's logs and storage to upload output artifacts
	hostPathDir     = apiv1.HostPathDirectory
	volumeDockerLib = apiv1.Volume{
		Name: common.DockerLibVolumeName,
		VolumeSource: apiv1.VolumeSource{
			HostPath: &apiv1.HostPathVolumeSource{
				Path: common.DockerLibHostPath,
				Type: &hostPathDir,
			},
		},
	}
	volumeMountDockerLib = apiv1.VolumeMount{
		Name:      volumeDockerLib.Name,
		MountPath: volumeDockerLib.VolumeSource.HostPath.Path,
		ReadOnly:  true,
	}

	// execEnvVars exposes various pod information as environment variables to the exec container
	execEnvVars = []apiv1.EnvVar{
		envFromField(common.EnvVarHostIP, "status.hostIP"),
		envFromField(common.EnvVarPodIP, "status.podIP"),
		envFromField(common.EnvVarPodName, "metadata.name"),
		envFromField(common.EnvVarNamespace, "metadata.namespace"),
	}
)

// envFromField is a helper to return a EnvVar with the name and field
func envFromField(envVarName, fieldPath string) apiv1.EnvVar {
	return apiv1.EnvVar{
		Name: envVarName,
		ValueFrom: &apiv1.EnvVarSource{
			FieldRef: &apiv1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  fieldPath,
			},
		},
	}
}

func (woc *wfOperationCtx) createWorkflowPod(nodeName string, tmpl *wfv1.Template) error {
	woc.log.Infof("Creating Pod: %s", nodeName)
	tmpl = tmpl.DeepCopy()
	waitCtr, err := woc.newWaitContainer(tmpl)
	if err != nil {
		return err
	}
	var mainCtr apiv1.Container
	if tmpl.Container != nil {
		mainCtr = *tmpl.Container
	} else if tmpl.Script != nil {
		// script case
		mainCtr = apiv1.Container{
			Image:   tmpl.Script.Image,
			Command: tmpl.Script.Command,
			Args:    []string{common.ScriptTemplateSourcePath},
		}
	} else {
		return errors.InternalError("Cannot create container from non-container/script template")
	}
	mainCtr.Name = common.MainContainerName
	t := true

	pod := apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: woc.wf.NodeID(nodeName),
			Labels: map[string]string{
				common.LabelKeyWorkflow:     woc.wf.ObjectMeta.Name, // Allow filtering by pods related to specific workflow
				common.LabelKeyArgoWorkflow: "true",                 // Allow filtering by only argo workflow related pods
			},
			Annotations: map[string]string{
				common.AnnotationKeyNodeName: nodeName,
			},
			OwnerReferences: []metav1.OwnerReference{
				metav1.OwnerReference{
					APIVersion:         wfv1.CRDFullName,
					Kind:               wfv1.CRDKind,
					Name:               woc.wf.ObjectMeta.Name,
					UID:                woc.wf.ObjectMeta.UID,
					BlockOwnerDeletion: &t,
				},
			},
		},
		Spec: apiv1.PodSpec{
			RestartPolicy: apiv1.RestartPolicyNever,
			Containers: []apiv1.Container{
				*waitCtr,
				mainCtr,
			},
			Volumes: []apiv1.Volume{
				volumePodMetadata,
				volumeDockerLib,
			},
		},
	}

	// Add init container only if it needs input artifacts
	// or if it is a script template (which needs to populate the script)
	if len(tmpl.Inputs.Artifacts) > 0 || tmpl.Script != nil {
		initCtr := woc.newInitContainer(tmpl)
		pod.Spec.InitContainers = []apiv1.Container{initCtr}
	}

	err = woc.addVolumeReferences(&pod, tmpl)
	if err != nil {
		return err
	}

	err = woc.addInputArtifactsVolumes(&pod, tmpl)
	if err != nil {
		return err
	}
	woc.addOutputArtifactsRepoMetaData(&pod, tmpl)

	if tmpl.Script != nil {
		addScriptVolume(&pod)
	}

	err = addSidecars(&pod, tmpl)
	if err != nil {
		return err
	}

	// Set the container template JSON in pod annotations, which executor will look to for artifact
	tmplBytes, err := json.Marshal(tmpl)
	if err != nil {
		return err
	}
	pod.ObjectMeta.Annotations[common.AnnotationKeyTemplate] = string(tmplBytes)

	created, err := woc.controller.podCl.Create(&pod)
	if err != nil {
		if apierr.IsAlreadyExists(err) {
			// workflow pod names are deterministic. We can get here if
			// the controller fails to persist the workflow after creating the pod.
			woc.log.Infof("pod %s already exists", nodeName)
			return nil
		}
		woc.log.Infof("Failed to create pod %s: %v", nodeName, err)
		return errors.InternalWrapError(err)
	}
	woc.log.Infof("Created pod: %s", created.Name)
	return nil
}

func (woc *wfOperationCtx) newInitContainer(tmpl *wfv1.Template) apiv1.Container {
	ctr := woc.newExecContainer(common.InitContainerName, false)
	ctr.Command = []string{"sh", "-c"}
	argoExecCmd := fmt.Sprintf("echo sleeping; cat %s; sleep 10; find /argo; echo done", common.PodMetadataAnnotationsPath)
	ctr.Args = []string{argoExecCmd}
	ctr.VolumeMounts = []apiv1.VolumeMount{
		volumeMountPodMetadata,
	}
	return *ctr
}

func (woc *wfOperationCtx) newWaitContainer(tmpl *wfv1.Template) (*apiv1.Container, error) {
	ctr := woc.newExecContainer(common.WaitContainerName, false)
	ctr.Command = []string{"sh", "-c"}
	argoExecCmd := fmt.Sprintf("echo sleeping; cat %s; sleep 10; echo done", common.PodMetadataAnnotationsPath)
	ctr.Args = []string{argoExecCmd}
	ctr.VolumeMounts = []apiv1.VolumeMount{
		volumeMountPodMetadata,
		volumeMountDockerLib,
	}
	return ctr, nil
}

func (woc *wfOperationCtx) newExecContainer(name string, privileged bool) *apiv1.Container {
	exec := apiv1.Container{
		Name:  name,
		Image: woc.controller.Config.ExecutorImage,
		Env:   execEnvVars,
		Resources: apiv1.ResourceRequirements{
			Limits: apiv1.ResourceList{
				apiv1.ResourceCPU:    resource.MustParse("0.5"),
				apiv1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Requests: apiv1.ResourceList{
				apiv1.ResourceCPU:    resource.MustParse("0.1"),
				apiv1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
		SecurityContext: &apiv1.SecurityContext{
			Privileged: &privileged,
		},
	}
	return &exec
}

// addVolumeReferences adds any volumeMounts that a container is referencing, to the pod.spec.volumes
// These are either specified in the workflow.spec.volumes or the workflow.spec.volumeClaimTemplate section
func (woc *wfOperationCtx) addVolumeReferences(pod *apiv1.Pod, tmpl *wfv1.Template) error {
	if tmpl.Container == nil {
		return nil
	}
	for _, volMnt := range tmpl.Container.VolumeMounts {
		vol := getVolByName(volMnt.Name, woc.wf)
		if vol == nil {
			return errors.Errorf(errors.CodeBadRequest, "volume '%s' not found in workflow spec", volMnt.Name)
		}
		if len(pod.Spec.Volumes) == 0 {
			pod.Spec.Volumes = make([]apiv1.Volume, 0)
		}
		pod.Spec.Volumes = append(pod.Spec.Volumes, *vol)
	}
	return nil
}

// getVolByName is a helper to retreive a volume by its name, either from the volumes or claims section
func getVolByName(name string, wf *wfv1.Workflow) *apiv1.Volume {
	for _, vol := range wf.Spec.Volumes {
		if vol.Name == name {
			return &vol
		}
	}
	for _, pvc := range wf.Status.PersistentVolumeClaims {
		if pvc.Name == name {
			return &pvc
		}
	}
	return nil
}

// addInputArtifactVolumes sets up the artifacts volume to the pod to support input artifacts to containers.
// In order support input artifacts, the init container shares a emptydir volume with the main container.
// It is the responsibility of the init container to load all artifacts to the mounted emptydir location.
// (e.g. /inputs/artifacts/CODE). The shared emptydir is mapped to the user's desired location in the main
// container.
//
// It is possible that a user specifies overlapping paths of an artifact path with a volume mount,
// (e.g. user wants an external volume mounted at /src, while simultaneously wanting an input artifact
// placed at /src/some/subdirectory). When this occurs, we need to prevent the duplicate bind mounting of
// overlapping volumes, since the outer volume will not see the changes made in the artifact emptydir.
//
// To prevent overlapping bind mounts, both the controller and executor will recognize the overlap between
// the explicit volume mount and the artifact emptydir and prevent all uses of the emptydir for purposes of
// loading data. The controller will omit mounting the emptydir to the artifact path, and the executor
// will load the artifact in the in user's volume (as opposed to the emptydir)
func (woc *wfOperationCtx) addInputArtifactsVolumes(pod *apiv1.Pod, tmpl *wfv1.Template) error {
	if len(tmpl.Inputs.Artifacts) == 0 {
		return nil
	}
	artVol := apiv1.Volume{
		Name: "input-artifacts",
		VolumeSource: apiv1.VolumeSource{
			EmptyDir: &apiv1.EmptyDirVolumeSource{},
		},
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, artVol)

	for i, initCtr := range pod.Spec.InitContainers {
		if initCtr.Name == common.InitContainerName {
			volMount := apiv1.VolumeMount{
				Name:      artVol.Name,
				MountPath: common.ExecutorArtifactBaseDir,
			}
			initCtr.VolumeMounts = append(initCtr.VolumeMounts, volMount)

			// We also add the user supplied mount paths to the init container,
			// in case the executor needs to load artifacts to this volume
			// instead of the artifacts volume
			for _, mnt := range tmpl.Container.VolumeMounts {
				mnt.MountPath = "/mainctrfs" + mnt.MountPath
				initCtr.VolumeMounts = append(initCtr.VolumeMounts, mnt)
			}

			// HACK: debug purposes. sleep to experiment with init container artifacts
			initCtr.Command = []string{"sh", "-c"}
			//initCtr.Args = []string{"argoexec artifacts load"}
			initCtr.Args = []string{"sleep 999999; echo done"}

			pod.Spec.InitContainers[i] = initCtr
			break
		}
	}

	mainCtrIndex := 0
	var mainCtr *apiv1.Container
	for i, ctr := range pod.Spec.Containers {
		if ctr.Name == common.MainContainerName {
			mainCtrIndex = i
			mainCtr = &ctr
		}
		if ctr.Name == common.WaitContainerName {
			// HACK: debug purposes. sleep to experiment with wait container artifacts
			ctr.Command = []string{"sh", "-c"}
			ctr.Args = []string{"sleep 999999; echo done"}
			pod.Spec.Containers[i] = ctr
		}
	}
	if mainCtr == nil {
		panic("Could not find main container in pod spec")
	}
	// TODO: the order in which we construct the volume mounts may matter,
	// especially if they are overlapping.
	for _, art := range tmpl.Inputs.Artifacts {
		if art.Path == "" {
			return errors.Errorf(errors.CodeBadRequest, "inputs.artifacts.%s did not specify a path", art.Name)
		}
		overlap := common.FindOverlappingVolume(tmpl, art.Path)
		if overlap != nil {
			// artifact path overlaps with a mounted volume. do not mount the
			// artifacts emptydir to the main container. init would have copied
			// the artifact to the user's volume instead
			woc.log.Debugf("skip volume mount of %s (%s): overlaps with mount %s at %s",
				art.Name, art.Path, overlap.Name, overlap.MountPath)
			continue
		}
		volMount := apiv1.VolumeMount{
			Name:      artVol.Name,
			MountPath: art.Path,
			SubPath:   art.Name,
		}
		if mainCtr.VolumeMounts == nil {
			mainCtr.VolumeMounts = make([]apiv1.VolumeMount, 0)
		}
		mainCtr.VolumeMounts = append(mainCtr.VolumeMounts, volMount)
	}
	pod.Spec.Containers[mainCtrIndex] = *mainCtr
	return nil
}

// addOutputArtifactsRepoMetaData updates the template with artifact repository information configured in the controller.
// This is skipped for artifacts which have explicitly set an output artifact location in the template
func (woc *wfOperationCtx) addOutputArtifactsRepoMetaData(pod *apiv1.Pod, tmpl *wfv1.Template) {
	for i, art := range tmpl.Outputs.Artifacts {
		if art.HasLocation() {
			// The artifact destination was explicitly set in the template. Skip
			continue
		}
		if woc.controller.Config.ArtifactRepository.S3 != nil {
			// artifacts are stored in S3 using the following formula:
			// <repo_key_prefix>/<worflow_name>/<node_id>/<artifact_name>
			// (e.g. myworkflowartifacts/argo-wf-fhljp/argo-wf-fhljp-123291312382/CODE)
			// TODO: will need to support more advanced organization of artifacts such as dated
			// (e.g. myworkflowartifacts/2017/10/31/... )
			keyPrefix := ""
			if woc.controller.Config.ArtifactRepository.S3.KeyPrefix != "" {
				keyPrefix = woc.controller.Config.ArtifactRepository.S3.KeyPrefix + "/"
			}
			artLocationKey := fmt.Sprintf("%s%s/%s/%s", keyPrefix, pod.Labels[common.LabelKeyWorkflow], pod.ObjectMeta.Name, art.Name)
			art.S3 = &wfv1.S3Artifact{
				S3Bucket: woc.controller.Config.ArtifactRepository.S3.S3Bucket,
				Key:      artLocationKey,
			}
		}
		tmpl.Outputs.Artifacts[i] = art
	}
}

// addScriptVolume sets up the shared volume between init container and main container
// containing the template script source code
func addScriptVolume(pod *apiv1.Pod) {
	volName := "script"
	scriptVol := apiv1.Volume{
		Name: volName,
		VolumeSource: apiv1.VolumeSource{
			EmptyDir: &apiv1.EmptyDirVolumeSource{},
		},
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, scriptVol)

	for i, initCtr := range pod.Spec.InitContainers {
		if initCtr.Name == common.InitContainerName {
			volMount := apiv1.VolumeMount{
				Name:      volName,
				MountPath: common.ScriptTemplateEmptyDir,
			}
			initCtr.VolumeMounts = append(initCtr.VolumeMounts, volMount)
			initCtr.Command = []string{"sh", "-c"}
			initCtr.Args = []string{"grep template /argo/podmetadata/annotations | cut -d = -f 2- | jq -rM '.' | jq -rM '.script.source' > /argo/script/source"}
			pod.Spec.InitContainers[i] = initCtr
			break
		}
	}
	found := false
	for i, ctr := range pod.Spec.Containers {
		if ctr.Name == common.MainContainerName {
			volMount := apiv1.VolumeMount{
				Name:      volName,
				MountPath: common.ScriptTemplateEmptyDir,
			}
			if ctr.VolumeMounts == nil {
				ctr.VolumeMounts = []apiv1.VolumeMount{volMount}
			} else {
				ctr.VolumeMounts = append(ctr.VolumeMounts, volMount)
			}
			pod.Spec.Containers[i] = ctr
			found = true
			break
		}
		if ctr.Name == common.WaitContainerName {
			ctr.Command = []string{"sh", "-c"}
			ctr.Args = []string{`
				while true ; do kubectl get pod $ARGO_POD_NAME -o custom-columns=status:status.containerStatuses[0].state.terminated 2>/dev/null; if [ $? -eq 0 ] ; then break; fi; echo waiting; sleep 5; done &&
				container_id=$(kubectl get pod $ARGO_POD_NAME -o jsonpath='{.status.containerStatuses[0].containerID}' | cut -d / -f 3-) &&
				output=$(grep stdout /var/lib/docker/containers/$container_id/*.log | jq -r '.log') &&
				outputjson={\"result\":\"$output\"} &&
				kubectl annotate pods $ARGO_POD_NAME --overwrite workflows.argoproj.io/outputs=${outputjson}
			`}
			pod.Spec.Containers[i] = ctr
		}
	}
	if !found {
		panic("Unable to locate main container")
	}
}

// addSidecars adds all sidecars to the pod spec of the step.
// Optionally volume mounts from the main container to the sidecar
func addSidecars(pod *apiv1.Pod, tmpl *wfv1.Template) error {
	if len(tmpl.Sidecars) == 0 {
		return nil
	}
	var mainCtr *apiv1.Container
	for _, ctr := range pod.Spec.Containers {
		if ctr.Name != common.MainContainerName {
			continue
		}
		mainCtr = &ctr
		break
	}
	if mainCtr == nil {
		panic("Unable to locate main container")
	}
	for _, sidecar := range tmpl.Sidecars {
		if sidecar.Options.VolumeMirroring != nil && *sidecar.Options.VolumeMirroring {
			for _, volMnt := range mainCtr.VolumeMounts {
				if sidecar.VolumeMounts == nil {
					sidecar.VolumeMounts = make([]apiv1.VolumeMount, 0)
				}
				sidecar.VolumeMounts = append(sidecar.VolumeMounts, volMnt)
			}
		}
		pod.Spec.Containers = append(pod.Spec.Containers, sidecar.Container)
	}
	return nil
}

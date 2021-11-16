package kubernetes

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
	transform "github.com/konveyor/crane-lib/transform"
	"github.com/konveyor/crane-lib/transform/util"
	"github.com/konveyor/crane-lib/transform/types"
	"github.com/konveyor/crane-lib/version"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
)

var logger logrus.FieldLogger

const (

	// flags
	AddAnnotations              = "add-annotations"
	RemoveAnnotations           = "remove-annotations"
	RegistryReplacement         = "registry-replacement"
	ExtraWhiteouts              = "extra-whiteouts"
	IncludeOnly                 = "include-only"
	DisableWhiteoutOwned        = "disable-whiteout-owned"

	containerImageUpdate        = "/spec/template/spec/containers/%v/image"
	initContainerImageUpdate    = "/spec/template/spec/initContainers/%v/image"
	podContainerImageUpdate     = "/spec/containers/%v/image"
	podInitContainerImageUpdate = "/spec/initContainers/%v/image"
	annotationInitial           = `%v
{"op": "add", "path": "/metadata/annotations/%v", "value": "%v"}`
	annotationNext = `%v,
{"op": "add", "path": "/metadata/annotations/%v", "value": "%v"}`
	removeAnnotationInitial = `%v
{"op": "remove", "path": "/metadata/annotations/%v"}`
	removeAnnotationNext = `%v,
{"op": "remove", "path": "/metadata/annotations/%v"}`

	opRemove = `[
{"op": "remove", "path": "%v"}
]`
	metadata             = "metadata"
	podNodeName          = "/spec/nodeName"
	podNodeSelector      = "/spec/nodeSelector"
	podPriority          = "/spec/priority"
	updateClusterIP      = "/spec/clusterIP"
	updateClusterIPs     = "/spec/clusterIPs"
	updateExternalIPs    = "/spec/externalIPs"
	updateNodePortString = "/spec/ports/%v/nodePort"
)
var fieldsToStrip = [...][]string{
		[]string{metadata, "uid"},
		[]string{metadata, "selfLink"},
		[]string{metadata, "resourceVersion"},
		[]string{metadata, "creationTimestamp"},
		[]string{metadata, "generation"},
		[]string{"status"},
	}

var endpointGK = schema.GroupKind{
	Group: "",
	Kind:  "Endpoints",
}

var endpointSliceGK = schema.GroupKind{
	Group: "discovery.k8s.io",
	Kind:  "EndpointSlice",
}

var pvcGK = schema.GroupKind{
	Group: "",
	Kind:  "PersistentVolumeClaim",
}

var podGK = schema.GroupKind{
	Group: "",
	Kind:  "Pod",
}

var serviceGK = schema.GroupKind{
	Group: "",
	Kind:  "Service",
}

var gksToWhiteout = []schema.GroupKind{
	endpointGK,
	endpointSliceGK,
	pvcGK,
}

type KubernetesTransformPlugin struct {
	AddAnnotations       map[string]string
	RemoveAnnotations    []string
	RegistryReplacement  map[string]string
	DisableWhiteoutOwned bool
	ExtraWhiteouts       []schema.GroupKind
	IncludeOnly          []schema.GroupKind
}

func (k *KubernetesTransformPlugin) setOptionalFields(extras map[string]string) {
	if len(extras[AddAnnotations]) > 0 {
		k.AddAnnotations = transform.ParseOptionalFieldMapVal(extras[AddAnnotations])
	}
	if len(extras[RemoveAnnotations]) > 0 {
		k.RemoveAnnotations = transform.ParseOptionalFieldSliceVal(extras[RemoveAnnotations])
	}
	if len(extras[RegistryReplacement]) > 0 {
		k.RegistryReplacement = transform.ParseOptionalFieldMapVal(extras[RegistryReplacement])
	}
	if len(extras[ExtraWhiteouts]) > 0 {
		k.ExtraWhiteouts = parseGroupKindSlice(transform.ParseOptionalFieldSliceVal(extras[ExtraWhiteouts]))
	}
	if len(extras[IncludeOnly]) > 0 {
		k.IncludeOnly = parseGroupKindSlice(transform.ParseOptionalFieldSliceVal(extras[IncludeOnly]))
	}
	if len(extras[DisableWhiteoutOwned]) > 0 {
		var err error
		k.DisableWhiteoutOwned, err = strconv.ParseBool(extras[DisableWhiteoutOwned])
		if err != nil {
			k.DisableWhiteoutOwned = false
		}
	}
}

func (k *KubernetesTransformPlugin) Run(request transform.PluginRequest) (transform.PluginResponse, error) {
	logger = logrus.New()
	k.setOptionalFields(request.Extras)
	resp := transform.PluginResponse{}
	// Set version in the future
	resp.Version = string(transform.V1)
	var err error
	resp.IsWhiteOut = k.getWhiteOuts(request.Unstructured)
	if resp.IsWhiteOut {
		return resp, err
	}
	resp.Patches, err = k.getKubernetesTransforms(request.Unstructured)
	return resp, err

}

func (k *KubernetesTransformPlugin) Metadata() transform.PluginMetadata {
	return transform.PluginMetadata{
		Name:            "KubernetesPlugin",
		Version:         version.Version,
		RequestVersion:  []transform.Version{transform.V1},
		ResponseVersion: []transform.Version{transform.V1},
		OptionalFields:  []transform.OptionalFields{
			{
				FlagName: AddAnnotations,
				Help:     "Annotations to add to each resource",
				Example:  "annotation1=value1,annotation2=value2",
			},
			{
				FlagName: RegistryReplacement,
				Help:     "Map of image registry paths to swap on transform, in the format original-registry1=target-registry1,original-registry2=target-registry2...",
				Example:  "docker-registry.default.svc:5000=image-registry.openshift-image-registry.svc:5000,docker.io/foo=quay.io/bar",
			},
			{
				FlagName: RemoveAnnotations,
				Help:     "Annotations to remove",
				Example:  "annotation1,annotation2",
			},
			{
				FlagName: DisableWhiteoutOwned,
				Help:     "Disable whiting out owned pods and pod template resources",
				Example:  "true",
			},
			{
				FlagName: ExtraWhiteouts,
				Help:     "Additional resources to whiteout specified as a comma-separated list of GroupKind strings.",
				Example:  "Deployment.apps,Service,Route.route.openshift.io",
			},
			{
				FlagName: IncludeOnly,
				Help:     "If specified, every resource not listed here will be a whiteout. extra-whiteouts is ignored when include-only is specified. Specified as a comma-separated list of GroupKind strings.",
				Example:  "Deployment.apps,Service,Route.route.openshift.io",
			},
		},
	}
}

var _ transform.Plugin = &KubernetesTransformPlugin{}

func (k *KubernetesTransformPlugin) getWhiteOuts(obj unstructured.Unstructured) bool {
	groupKind := obj.GroupVersionKind().GroupKind()
	if len(k.IncludeOnly) > 0 {
		if !groupKindInList(groupKind, k.IncludeOnly) {
			return true
		}
	} else {
		if groupKindInList(groupKind, gksToWhiteout) {
			return true
		}
		if groupKindInList(groupKind, k.ExtraWhiteouts) {
			return true
		}
	}
	if k.DisableWhiteoutOwned {
		return false
	}
	_, isPodSpecable := types.IsPodSpecable(obj)
	if (groupKind == podGK || isPodSpecable) && len(obj.GetOwnerReferences()) > 0 {
		return true
	}
	return false
}

func parseGroupKindSlice(gkStrings []string) []schema.GroupKind {
	gks := []schema.GroupKind{}
	for _, gk := range gkStrings {
		gks = append(gks, schema.ParseGroupKind(gk))
	}
	return gks
}

func groupKindInList(gk schema.GroupKind, list []schema.GroupKind) bool {
	for _, thisGK := range list {
		if gk == thisGK {
			return true
		}
	}
	return false
}

func (k *KubernetesTransformPlugin) getKubernetesTransforms(obj unstructured.Unstructured) (jsonpatch.Patch, error) {

	// Always attempt to add annotations for each thing.
	jsonPatch := jsonpatch.Patch{}
	patches, err := stripFields(obj)
	if err != nil {
		return nil, err
	}
	jsonPatch = append(jsonPatch, patches...)
	if k.AddAnnotations != nil && len(k.AddAnnotations) > 0 {
		patches, err := addAnnotations(k.AddAnnotations)
		if err != nil {
			return nil, err
		}
		jsonPatch = append(jsonPatch, patches...)
	}
	if len(k.RemoveAnnotations) > 0 {
		patches, err := removeAnnotations(k.RemoveAnnotations)
		if err != nil {
			return nil, err
		}
		jsonPatch = append(jsonPatch, patches...)
	}
	if podGK == obj.GetObjectKind().GroupVersionKind().GroupKind() {
		patches, err := removePodFields()
		if err != nil {
			return nil, err
		}
		jsonPatch = append(jsonPatch, patches...)
	}
	if k.RegistryReplacement != nil && len(k.RegistryReplacement) > 0 {
		if podGK == obj.GetObjectKind().GroupVersionKind().GroupKind() {
			js, err := obj.MarshalJSON()
			if err != nil {
				return nil, err
			}
			pod := &v1.Pod{}
			err = json.Unmarshal(js, pod)
			if err != nil {
				return nil, err
			}
			jps := jsonpatch.Patch{}
			for i, container := range pod.Spec.Containers {
				updatedImage, update := util.UpdateImageRegistry(k.RegistryReplacement, container.Image)
				if update {
					jp, err := util.UpdateImage(fmt.Sprintf(podContainerImageUpdate, i), updatedImage)
					if err != nil {
						return nil, err
					}
					jps = append(jps, jp...)
				}
			}
			for i, container := range pod.Spec.InitContainers {
				updatedImage, update := util.UpdateImageRegistry(k.RegistryReplacement, container.Image)
				if update {
					jp, err := util.UpdateImage(fmt.Sprintf(podInitContainerImageUpdate, i), updatedImage)
					if err != nil {
						return nil, err
					}
					jps = append(jps, jp...)
				}
			}
			jsonPatch = append(jsonPatch, jps...)
		} else if template, ok := types.IsPodSpecable(obj); ok {
			jps := jsonpatch.Patch{}
			for i, container := range template.Spec.Containers {
				updatedImage, update := util.UpdateImageRegistry(k.RegistryReplacement, container.Image)
				if update {
					jp, err := util.UpdateImage(fmt.Sprintf(containerImageUpdate, i), updatedImage)
					if err != nil {
						return nil, err
					}
					jps = append(jps, jp...)
				}
			}
			for i, container := range template.Spec.InitContainers {
				updatedImage, update := util.UpdateImageRegistry(k.RegistryReplacement, container.Image)
				if update {
					jp, err := util.UpdateImage(fmt.Sprintf(initContainerImageUpdate, i), updatedImage)
					if err != nil {
						return nil, err
					}
					jps = append(jps, jp...)
				}
			}
			jsonPatch = append(jsonPatch, jps...)
		}
	}
	if obj.GetObjectKind().GroupVersionKind().GroupKind() == serviceGK {
		patches, err := removeServiceFields(obj)
		if err != nil {
			return nil, err
		}
		jsonPatch = append(jsonPatch, patches...)
	}

	return jsonPatch, nil
}

func interfaceSlice(inStrings []string) []interface{} {
	var outSlice []interface{}
	for _, str := range inStrings {
		outSlice = append(outSlice, str)
	}
	return outSlice
}
func stripFields(obj unstructured.Unstructured) (jsonpatch.Patch, error) {
	var patches jsonpatch.Patch
	for _, field := range fieldsToStrip {
		_, found, err := unstructured.NestedFieldNoCopy(obj.Object, field...)
		if err != nil {
			return patches, err
		}
		if found {
			patch, err := jsonpatch.DecodePatch([]byte(fmt.Sprintf(opRemove, fmt.Sprintf(strings.Repeat("/%v", len(field)), interfaceSlice(field)...))))
			if err != nil {
				return nil, err
			}
			patches = append(patches, patch...)
		}
	}
	return patches, nil
}

func addAnnotations(addAnnotations map[string]string) (jsonpatch.Patch, error) {
	patchJSON := `[`
	i := 0
	for key, value := range addAnnotations {
		if i == 0 {
			patchJSON = fmt.Sprintf(annotationInitial, patchJSON, key, value)
		} else {
			patchJSON = fmt.Sprintf(annotationNext, patchJSON, key, value)
		}
		i++
	}

	patchJSON = fmt.Sprintf("%v]", patchJSON)
	patch, err := jsonpatch.DecodePatch([]byte(patchJSON))
	if err != nil {
		fmt.Printf("%v", patchJSON)
		return nil, err
	}
	return patch, nil
}

func removeAnnotations(removeAnnotations []string) (jsonpatch.Patch, error) {
	patchJSON := `[`
	i := 0
	for _, annotation := range removeAnnotations {
		if i == 0 {
			patchJSON = fmt.Sprintf(removeAnnotationInitial, patchJSON, annotation)
		} else {
			patchJSON = fmt.Sprintf(removeAnnotationNext, patchJSON, annotation)
		}
		i++
	}

	patchJSON = fmt.Sprintf("%v]", patchJSON)
	patch, err := jsonpatch.DecodePatch([]byte(patchJSON))
	if err != nil {
		fmt.Printf("%v", patchJSON)
		return nil, err
	}
	return patch, nil
}

func removePodFields() (jsonpatch.Patch, error) {
	var patches jsonpatch.Patch
	patches, err := jsonpatch.DecodePatch([]byte(fmt.Sprintf(opRemove, podNodeName)))
	if err != nil {
		return nil, err
	}
	patch, err := jsonpatch.DecodePatch([]byte(fmt.Sprintf(opRemove, podNodeSelector)))
	if err != nil {
		return nil, err
	}
	patches = append(patches, patch...)
	patch, err = jsonpatch.DecodePatch([]byte(fmt.Sprintf(opRemove, podPriority)))
	if err != nil {
		return nil, err
	}
	patches = append(patches, patch...)
	return patches, nil
}

func removeServiceFields(obj unstructured.Unstructured) (jsonpatch.Patch, error) {
	var patches jsonpatch.Patch
	if isLoadBalancerService(obj) {
		patch, err := jsonpatch.DecodePatch([]byte(fmt.Sprintf(opRemove, updateExternalIPs)))
		if err != nil {
			return nil, err
		}
		patches = append(patches, patch...)
	}

	if shouldRemoveServiceClusterIP(obj) {
		patch, err := jsonpatch.DecodePatch([]byte(fmt.Sprintf(opRemove, updateClusterIP)))
		if err != nil {
			return nil, err
		}
		patches = append(patches, patch...)
	}
	if shouldRemoveServiceClusterIPs(obj) {
		patch, err := jsonpatch.DecodePatch([]byte(fmt.Sprintf(opRemove, updateClusterIPs)))
		if err != nil {
			return nil, err
		}
		patches = append(patches, patch...)
	}
	patch, err := getNodePortPatch(obj)
	if err != nil {
		return nil, err
	}
	patches = append(patches, patch...)
	return patches, nil
}

func isLoadBalancerService(u unstructured.Unstructured) bool {
	// Get Spec
	spec, ok := u.UnstructuredContent()["spec"]
	if !ok {
		return false
	}

	specMap, ok := spec.(map[string]interface{})
	if !ok {
		return false
	}
	// Get type
	serviceType, ok := specMap["type"]
	if !ok {
		return false
	}
	return serviceType == "LoadBalancer"
}

func shouldRemoveServiceClusterIP(u unstructured.Unstructured) bool {
	// Get Spec
	spec, ok := u.UnstructuredContent()["spec"]
	if !ok {
		return false
	}

	specMap, ok := spec.(map[string]interface{})
	if !ok {
		return false
	}
	clusterIP, ok := specMap["clusterIP"]
	if !ok {
		return false
	}
	return clusterIP != "None"
}

func shouldRemoveServiceClusterIPs(u unstructured.Unstructured) bool {
	// Get Spec
	spec, ok := u.UnstructuredContent()["spec"]
	if !ok {
		return false
	}

	specMap, ok := spec.(map[string]interface{})
	if !ok {
		return false
	}
	clusterIPs, ok := specMap["clusterIPs"]
	if !ok {
		return false
	}
	// At this point, we have clusterIPs. Remove unless there's a first None element

	clusterIPsSlice, ok := clusterIPs.([]interface{})
	if !ok {
		return true
	}
	if len(clusterIPsSlice) == 0 {
		return true
	}
	return (clusterIPsSlice[0] != "None")
}

func getNodePortPatch(u unstructured.Unstructured) (jsonpatch.Patch, error) {
	var patch jsonpatch.Patch
	// Get Spec
	spec, ok := u.UnstructuredContent()["spec"]
	if !ok {
		return patch, nil
	}
	specMap, ok := spec.(map[string]interface{})
	if !ok {
		return patch, nil
	}
	// Get type
	serviceType, ok := specMap["type"]
	if !ok {
		return patch, nil
	}
	if serviceType == "ExternalName" {
		return patch, nil
	}
	servicePorts, ok := specMap["ports"]
	if !ok {
		return patch, nil
	}
	portsSlice, ok := servicePorts.([]interface{})
	if !ok {
		return patch, nil
	}

	explicitNodePorts := sets.NewString()
	unnamedPortInts := sets.NewInt()
	lastAppliedConfig, ok := u.GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"]
	if ok {
		appliedServiceUnstructured := new(map[string]interface{})
		if err := json.Unmarshal([]byte(lastAppliedConfig), appliedServiceUnstructured); err != nil {
			return patch, err
		}

		ports, bool, err := unstructured.NestedSlice(*appliedServiceUnstructured, "spec", "ports")

		if err != nil {
			return patch, err
		}

		if bool {
			for _, port := range ports {
				p, ok := port.(map[string]interface{})
				if !ok {
					continue
				}
				nodePort, nodePortBool, err := unstructured.NestedFieldNoCopy(p, "nodePort")
				if err != nil {
					return patch, err
				}
				if nodePortBool {
					nodePortInt, err := getNodePortInt(nodePort)
					if err != nil {
						return patch, err
					}
					if nodePortInt > 0 {
						portName, ok := p["name"]
						if !ok {
							// unnamed port
							unnamedPortInts.Insert(nodePortInt)
						} else {
							explicitNodePorts.Insert(portName.(string))
						}

					}
				}
			}
		}
	}

	for i, portInterface := range portsSlice {
		removeNodePort := false
		var nameStr string
		port, ok := portInterface.(map[string]interface{})
		if !ok {
			continue
		}
		name, ok := port["name"]
		if ok {
			nameStr, _ = name.(string)
		}
		nodePort, ok := port["nodePort"]
		if !ok {
			continue
		}
		nodePortInt, err := getNodePortInt(nodePort)
		if err != nil {
			return patch, err
		}
		if nodePortInt == 0 {
			continue
		}
		if len(nameStr) > 0 {
			if !explicitNodePorts.Has(nameStr) {
				removeNodePort = true
			}
		} else {
			if !unnamedPortInts.Has(int(nodePortInt)) {
				removeNodePort = true
			}
		}
		if removeNodePort {
			patchJSON := fmt.Sprintf(opRemove, fmt.Sprintf(updateNodePortString, i))
			intPatch, err := jsonpatch.DecodePatch([]byte(patchJSON))
			if err != nil {
				return patch, err
			}
			patch = append(patch, intPatch...)
		}
	}

	return patch, nil
}

func getNodePortInt(nodePort interface{}) (int, error) {
	nodePortInt := 0
	switch nodePort.(type) {
	case int:
		nodePortInt = nodePort.(int)
	case int32:
		nodePortInt = int(nodePort.(int32))
	case int64:
		nodePortInt = int(nodePort.(int64))
	case float64:
		nodePortInt = int(nodePort.(float64))
	case string:
		nodePortInt, err := strconv.Atoi(nodePort.(string))
		if err != nil {
			return nodePortInt, err
		}
	}
	return nodePortInt, nil
}

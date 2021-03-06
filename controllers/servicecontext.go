package controllers

import (
	"sort"

	"github.com/imdario/mergo"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	v1alpha1 "github.com/redhat-developer/service-binding-operator/api/v1alpha1"
	"github.com/redhat-developer/service-binding-operator/pkg/binding"
	"github.com/redhat-developer/service-binding-operator/pkg/log"
)

// serviceContext contains information related to a service.
type serviceContext struct {
	// service is the resource of the service being evaluated.
	service *unstructured.Unstructured
	// envVars contains the service's contributed environment variables.
	envVars map[string]interface{}
	// namingTemplate contains name template used to prepare custom env variable key.
	namingTemplate string
	bindAsFiles    bool
	// Id indicates a name the service can be referred in custom environment variables.
	id *string
}

// serviceContextList is a list of ServiceContext values.
type serviceContextList []*serviceContext

// getServices returns a slice of service unstructured objects contained in the collection.
func (sc serviceContextList) getServices() []*unstructured.Unstructured {
	var crs []*unstructured.Unstructured
	for _, s := range sc {
		crs = append(crs, s.service)
	}
	return crs
}

func stringValueOrDefault(val *string, defaultVal string) string {
	if val != nil && len(*val) > 0 {
		return *val
	}
	return defaultVal
}

// buildServiceContexts return a collection of ServiceContext values from the given service
// selectors.
func buildServiceContexts(
	logger *log.Log,
	client dynamic.Interface,
	defaultNs string,
	selectors []v1alpha1.Service,
	includeServiceOwnedResources *bool,
	isBindAsFiles bool,
	typeLookup K8STypeLookup,
	namingTemplate string,
) (serviceContextList, error) {
	svcCtxs := make(serviceContextList, 0)

SELECTORS:
	for _, s := range selectors {
		ns := stringValueOrDefault(s.Namespace, defaultNs)
		gvr, err := typeLookup.ResourceForReferable(&s)
		if err != nil {
			return nil, err
		}
		gvk, err := typeLookup.KindForResource(*gvr)

		if err != nil {
			return nil, err
		}

		svcCtx, err := buildServiceContext(logger.WithName("buildServiceContexts"), client, ns, *gvk,
			s.Name, namingTemplate, isBindAsFiles, typeLookup, s.Id)

		if err != nil {
			// best effort approach; should not break in common cases such as a unknown annotation
			// prefix (other annotations might exist in the resource) or, in the case of a valid
			// annotation, the handler expected for the annotation can't be found.
			if binding.IsErrEmptyAnnotationName(err) || binding.IsErrHandlerNotFound(err) {
				logger.Trace("Continuing to next selector", "Error", err)
				continue SELECTORS
			}
			return nil, err
		}
		svcCtxs = append(svcCtxs, svcCtx)

		if includeServiceOwnedResources != nil && *includeServiceOwnedResources {
			// use the selector's kind as owned resources environment variable prefix
			ownedResourcesCtxs, err := findOwnedResourcesCtxs(
				logger,
				client,
				ns,
				svcCtx.service.GetUID(),
				namingTemplate,
				isBindAsFiles,
				typeLookup,
			)
			if err != nil {
				return nil, err
			}
			svcCtxs = append(svcCtxs, ownedResourcesCtxs...)
		}
	}

	return svcCtxs, nil
}

func findOwnedResourcesCtxs(
	logger *log.Log,
	client dynamic.Interface,
	ns string,
	uid types.UID,
	namingTemplate string,
	isBindAsFiles bool,
	typeLookup K8STypeLookup,
) (serviceContextList, error) {
	ownedResources, err := getOwnedResources(
		logger,
		client,
		ns,
		uid,
	)
	if err != nil {
		return nil, err
	}

	return buildOwnedResourceContexts(
		client,
		ownedResources,
		namingTemplate,
		isBindAsFiles,
		typeLookup,
	)
}

func merge(dst map[string]interface{}, src map[string]interface{}) (map[string]interface{}, error) {
	merged := map[string]interface{}{}

	err := mergo.Merge(&merged, src, mergo.WithOverride, mergo.WithOverrideEmptySlice)
	if err != nil {
		return nil, err
	}

	err = mergo.Merge(&merged, dst)
	if err != nil {
		return nil, err
	}

	return merged, nil
}

func runHandler(
	client dynamic.Interface,
	obj *unstructured.Unstructured,
	outputObj *unstructured.Unstructured,
	key string,
	value string,
	envVars map[string]interface{},
) error {
	h, err := binding.NewSpecHandler(client, key, value, *obj)
	if err != nil {
		return err
	}
	r, err := h.Handle()
	if err != nil {
		return err
	}

	if newObj, err := merge(outputObj.Object, r.RawData); err != nil {
		return err
	} else {
		outputObj.Object = newObj
	}

	err = mergo.Merge(&envVars, r.Data, mergo.WithAppendSlice, mergo.WithOverride)
	if err != nil {
		return err
	}

	return nil
}

// buildServiceContext inspects g the API server searching for the service resources, associated CRD
// and OLM's CRDDescription if present, and processes those with relevant annotations to compose a
// ServiceContext.
func buildServiceContext(
	logger *log.Log,
	client dynamic.Interface,
	ns string,
	gvk schema.GroupVersionKind,
	name string,
	namingTemplate string,
	bindAsFiles bool,
	typeLookup K8STypeLookup,
	id *string,
) (*serviceContext, error) {
	gvr, err := typeLookup.ResourceForKind(gvk)
	if err != nil {
		return nil, err
	}
	obj, err := findService(client, ns, *gvr, name)
	if err != nil {
		return nil, err
	}

	anns := map[string]string{}

	// attempt to search the CRD of given gvk and bail out right away if a CRD can't be found; this
	// means also a CRDDescription can't exist or if it does exist it is not meaningful.
	crd, err := findServiceCRD(client, *gvr)
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	} else if !errors.IsNotFound(err) {
		// attempt to search the a CRDDescription related to the obtained CRD.
		crdDescription, err := findCRDDescription(ns, client, gvk, crd)
		if err != nil && !errors.IsNotFound(err) {
			return nil, err
		}
		// start with annotations extracted from CRDDescription
		err = mergo.Merge(
			&anns, convertCRDDescriptionToAnnotations(crdDescription), mergo.WithOverride)
		if err != nil {
			return nil, err
		}
		// then override collected annotations with CRD annotations
		err = mergo.Merge(&anns, crd.GetAnnotations(), mergo.WithOverride)
		if err != nil {
			return nil, err
		}
	}

	// and finally override collected annotations with own annotations
	err = mergo.Merge(&anns, obj.GetAnnotations(), mergo.WithOverride)
	if err != nil {
		return nil, err
	}

	envVars := make(map[string]interface{})

	// outputObj will be used to keep the changes processed by the handler.
	outputObj := obj.DeepCopy()

	keys := make([]string, 0)
	for k := range anns {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		v := anns[k]
		// runHandler modifies 'outputObj', and 'envVars' in place.
		err := runHandler(client, obj, outputObj, k, v, envVars)
		if err != nil {
			logger.Debug("Failed executing runHandler in envars", "Error", err)
		}
	}

	serviceCtx := &serviceContext{
		service:        outputObj,
		envVars:        envVars,
		namingTemplate: namingTemplate,
		bindAsFiles:    bindAsFiles,
		id:             id,
	}

	return serviceCtx, nil
}

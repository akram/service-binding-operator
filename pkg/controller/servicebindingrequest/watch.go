package servicebindingrequest

import (
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// CSVToWatcherMapper creates a EventHandler interface to map ClusterServiceVersion objects back to
// controller and add given GVK to watch list.
type CSVToWatcherMapper struct {
	controller *SBRController
}

// Map requests directed to CSV objects and exctract related GVK to trigger another watch on
// controller instance.
func (c *CSVToWatcherMapper) Map(obj handler.MapObject) []reconcile.Request {
	olm := NewOLM(c.controller.Client, obj.Meta.GetNamespace())
	namespacedName := types.NamespacedName{
		Namespace: obj.Meta.GetNamespace(),
		Name:      obj.Meta.GetName(),
	}

	logger := log.WithName("CSVToWatcherMapper").WithValues("Obj.NamespacedName", namespacedName)

	gvks, err := olm.ListGVKsFromCSVNamespacedName(namespacedName)
	if err != nil {
		logger.Error(err, "Failed on listing GVK with namespaced-name!")
		return []reconcile.Request{}
	}

	for _, gvk := range gvks {
		logger = logger.WithValues("GVK", gvk)
		logger.Info("Adding watch for GVK")
		err = c.controller.AddWatchForGVK(gvk)
		if err != nil {
			logger.Error(err, "Failed to create a watch")
		}
	}

	return []reconcile.Request{}
}

// NewCreateWatchEventHandler creates a new instance of handler.EventHandler interface with
// CSVToWatcherMapper as map-func.
func NewCreateWatchEventHandler(controller *SBRController) handler.EventHandler {
	return &handler.EnqueueRequestsFromMapFunc{
		ToRequests: &CSVToWatcherMapper{controller: controller},
	}
}

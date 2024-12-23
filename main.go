package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"golang.org/x/exp/slog"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// Interfaces and Structs

type ResourceControllerInterface interface {
	GetGVR() schema.GroupVersionResource
	AddFunc(interface{})
	UpdateFunc(interface{}, interface{})
	DeleteFunc(interface{})
}

type ResourceController struct {
	GVR          schema.GroupVersionResource
	Logger       *slog.Logger
	includePaths []string
	excludePaths []string
	namespaces   []string
}

func NewResourceController(
	group, version, resource string,
	logger *slog.Logger,
	includePaths, excludePaths, namespaces []string,
) *ResourceController {
	return &ResourceController{
		GVR:          schema.GroupVersionResource{Group: group, Version: version, Resource: resource},
		Logger:       logger.With("group", group).With("version", version, "kind", resource),
		includePaths: includePaths,
		excludePaths: excludePaths,
		namespaces:   namespaces,
	}
}

func (rc *ResourceController) NamespaceMatches(unstructuredObj *unstructured.Unstructured) bool {
	if len(unstructuredObj.GetNamespace()) == 0 {
		return true
	}
	if len(rc.namespaces) == 0 {
		return true
	}
	for _, ns := range rc.namespaces {
		if unstructuredObj.GetNamespace() == ns {
			return true
		}
	}
	return false
}

// ResourceController methods

func (rc *ResourceController) GetGVR() schema.GroupVersionResource {
	return rc.GVR
}

func (rc *ResourceController) AddFunc(obj interface{}) {
	objUnstructured := obj.(*unstructured.Unstructured)
	if rc.NamespaceMatches(objUnstructured) {
		rc.handleEvent("Add", objUnstructured)
	}
}

func (rc *ResourceController) UpdateFunc(oldObj, newObj interface{}) {
	oldUnstructured := oldObj.(*unstructured.Unstructured)
	newUnstructured := newObj.(*unstructured.Unstructured)
	if !rc.NamespaceMatches(newUnstructured) {
		return
	}
	if !reflect.DeepEqual(rc.filterObject(oldUnstructured), rc.filterObject(newUnstructured)) {
		rc.handleEvent("Update", newUnstructured)
	}
}

func (rc *ResourceController) DeleteFunc(obj interface{}) {
	objUnstructured := obj.(*unstructured.Unstructured)
	if rc.NamespaceMatches(objUnstructured) {
		rc.handleEvent("Delete", objUnstructured)
	}
}

func (rc *ResourceController) filterObject(obj *unstructured.Unstructured) *unstructured.Unstructured {
	filteredObj := obj.DeepCopy()
	if len(rc.includePaths) > 0 {
		filteredObj = &unstructured.Unstructured{Object: make(map[string]interface{})}
		for _, path := range rc.includePaths {
			value, found, _ := unstructured.NestedFieldCopy(obj.Object, strings.Split(path, ".")...)
			if found {
				unstructured.SetNestedField(filteredObj.Object, value, strings.Split(path, ".")...)
			}
		}
	}
	for _, path := range rc.excludePaths {
		unstructured.RemoveNestedField(filteredObj.Object, strings.Split(path, ".")...)
	}
	return filteredObj
}

func (rc *ResourceController) handleEvent(eventType string, unstructuredObj *unstructured.Unstructured) {
	filteredObj := rc.filterObject(unstructuredObj)
	rc.Logger.Info("Event", "eventType", eventType, "obj", filteredObj.Object)
}

// Client and Informer setup

func createDynamicClient() (dynamic.Interface, error) {
	var config *rest.Config
	var err error

	kubeConfig := os.Getenv("KUBECONFIG")
	if kubeConfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
		if err == nil {
			return dynamic.NewForConfig(config)
		}
	}

	homeDir, _ := os.UserHomeDir()
	defaultKubeConfig := filepath.Join(homeDir, ".kube", "config")
	config, err = clientcmd.BuildConfigFromFlags("", defaultKubeConfig)
	if err == nil {
		return dynamic.NewForConfig(config)
	}

	// Если не удалось с предыдущими, пробуем получить конфиг из кластера.
	config, err = rest.InClusterConfig()
	if err == nil {
		return dynamic.NewForConfig(config)
	}

	return nil, nil
}

func setupInformers(client dynamic.Interface, controllers []ResourceControllerInterface) []cache.SharedIndexInformer {
	informers := make([]cache.SharedIndexInformer, len(controllers))
	for i, controller := range controllers {
		informer := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, time.Second, corev1.NamespaceAll, nil).
			ForResource(controller.GetGVR()).Informer()
		informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    controller.AddFunc,
			UpdateFunc: controller.UpdateFunc,
			DeleteFunc: controller.DeleteFunc,
		})
		informers[i] = informer
	}
	return informers
}

func informersSyncedCallback(informers []cache.SharedIndexInformer) cache.InformerSynced {
	return func() bool {
		for _, informer := range informers {
			if !informer.HasSynced() {
				return false
			}
		}
		return true
	}
}

// Configuration Structures

type FilterConfig struct {
	IncludePaths []string `yaml:"includePaths"`
	ExcludePaths []string `yaml:"excludePaths"`
	Namespaces   []string `yaml:"namespaces"`
}

type CommonConfig struct {
	FilterConfig `yaml:",inline"`
}

type ResourceConfig struct {
	Group        string `yaml:"group"`
	Version      string `yaml:"version"`
	Resource     string `yaml:"resource"`
	FilterConfig `yaml:",inline"`
}

type Config struct {
	Common    CommonConfig     `yaml:"common"`
	Resources []ResourceConfig `yaml:"resources"`
}

// Main function

func main() {
	// Define a flag for the config file path
	configFilePath := flag.String("config", "config.yaml", "path to the configuration file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Load and parse configuration
	data, err := os.ReadFile(*configFilePath)
	if err != nil {
		logger.Error("Failed to read config.yaml", "error", err)
		os.Exit(1)
	}
	var config Config
	if err = yaml.Unmarshal(data, &config); err != nil {
		logger.Error("Failed to unmarshal config.yaml", "error", err)
		os.Exit(1)
	}

	// Setup Resource Controllers
	var controllers []ResourceControllerInterface
	for _, resConfig := range config.Resources {
		controller := NewResourceController(
			resConfig.Group,
			resConfig.Version,
			resConfig.Resource,
			logger,
			append(config.Common.IncludePaths, resConfig.IncludePaths...),
			append(config.Common.ExcludePaths, resConfig.ExcludePaths...),
			append(config.Common.Namespaces, resConfig.Namespaces...),
		)
		controllers = append(controllers, controller)
	}

	// Setup Dynamic Client and Informers
	client, err := createDynamicClient()
	if err != nil {
		logger.Error("Failed to create dynamic client", "error", err)
		os.Exit(1)
	}
	informers := setupInformers(client, controllers)

	// Run Informers
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	for _, informer := range informers {
		go informer.Run(ctx.Done())
	}
	logger.Info("Waiting for cache sync...")
	if !cache.WaitForCacheSync(ctx.Done(), informersSyncedCallback(informers)) {
		logger.Error("Failed to sync cache")
		os.Exit(1)
	}
	logger.Info("Cache synced successfully")
	<-ctx.Done()
	logger.Info("Shutting down gracefully...")
}

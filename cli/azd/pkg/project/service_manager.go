// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/azure/azure-dev/cli/azd/pkg/alpha"
	"github.com/azure/azure-dev/cli/azd/pkg/async"
	"github.com/azure/azure-dev/cli/azd/pkg/azapi"
	"github.com/azure/azure-dev/cli/azd/pkg/environment"
	"github.com/azure/azure-dev/cli/azd/pkg/ext"
	"github.com/azure/azure-dev/cli/azd/pkg/ioc"
	"github.com/azure/azure-dev/cli/azd/pkg/osutil"
	"github.com/azure/azure-dev/cli/azd/pkg/tools"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/swa"
)

const (
	ServiceEventEnvUpdated ext.Event = "environment updated"
	ServiceEventRestore    ext.Event = "restore"
	ServiceEventBuild      ext.Event = "build"
	ServiceEventPackage    ext.Event = "package"
	ServiceEventDeploy     ext.Event = "deploy"
)

var (
	ServiceEvents []ext.Event = []ext.Event{
		ServiceEventEnvUpdated,
		ServiceEventRestore,
		ServiceEventPackage,
		ServiceEventDeploy,
	}
)

// ServiceManager provides a management layer for performing operations against an azd service within a project
// The component performs all of the heavy lifting for executing all lifecycle operations for a service.
//
// All service lifecycle command leverage our async Task library to expose a common interface for handling
// long running operations including how we handle incremental progress updates and error handling.
type ServiceManager interface {
	// Gets all of the required framework/service target tools for the specified service config
	GetRequiredTools(ctx context.Context, serviceConfig *ServiceConfig) ([]tools.ExternalTool, error)

	// Initializes the service configuration and dependent framework & service target
	// This allows frameworks & service targets to hook into a services lifecycle events
	Initialize(ctx context.Context, serviceConfig *ServiceConfig) error

	// Restores the code dependencies for the specified service config
	Restore(
		ctx context.Context,
		serviceConfig *ServiceConfig,
		progress *async.Progress[ServiceProgress],
	) (*ServiceRestoreResult, error)

	// Builds the code for the specified service config
	// Will call the language compile for compiled languages or
	// may copy build artifacts to a configured output folder
	Build(
		ctx context.Context,
		serviceConfig *ServiceConfig,
		restoreOutput *ServiceRestoreResult,
		progress *async.Progress[ServiceProgress],
	) (*ServiceBuildResult, error)

	// Packages the code for the specified service config
	// Depending on the service configuration this will generate an artifact
	// that can be consumed by the hosting Azure service.
	// Common examples could be a zip archive for app service or
	// Docker images for container apps and AKS
	Package(
		ctx context.Context,
		serviceConfig *ServiceConfig,
		buildOutput *ServiceBuildResult,
		progress *async.Progress[ServiceProgress],
		options *PackageOptions,
	) (*ServicePackageResult, error)

	// Deploys the generated artifacts to the Azure resource that will
	// host the service application
	// Common examples would be uploading zip archive using ZipDeploy deployment or
	// pushing container images to a container registry.
	Deploy(
		ctx context.Context,
		serviceConfig *ServiceConfig,
		packageOutput *ServicePackageResult,
		progress *async.Progress[ServiceProgress],
	) (*ServiceDeployResult, error)

	// Gets the framework service for the specified service config
	// The framework service performs the restoration and building of the service app code
	GetFrameworkService(ctx context.Context, serviceConfig *ServiceConfig) (FrameworkService, error)

	// Gets the service target service for the specified service config
	// The service target is responsible for packaging & deploying the service app code
	// to the destination Azure resource
	GetServiceTarget(ctx context.Context, serviceConfig *ServiceConfig) (ServiceTarget, error)
}

// ServiceOperationCache is an alias to map used for internal caching of service operation results
// The ServiceManager is a scoped component since it depends on the current environment
// The ServiceOperationCache is used as a singleton cache for all service manager instances
type ServiceOperationCache map[string]any

type serviceManager struct {
	env                 *environment.Environment
	resourceManager     ResourceManager
	serviceLocator      ioc.ServiceLocator
	operationCache      ServiceOperationCache
	alphaFeatureManager *alpha.FeatureManager
	initialized         map[*ServiceConfig]map[any]bool
}

// NewServiceManager creates a new instance of the ServiceManager component
func NewServiceManager(
	env *environment.Environment,
	resourceManager ResourceManager,
	serviceLocator ioc.ServiceLocator,
	operationCache ServiceOperationCache,
	alphaFeatureManager *alpha.FeatureManager,
) ServiceManager {
	return &serviceManager{
		env:                 env,
		resourceManager:     resourceManager,
		serviceLocator:      serviceLocator,
		operationCache:      operationCache,
		alphaFeatureManager: alphaFeatureManager,
		initialized:         map[*ServiceConfig]map[any]bool{},
	}
}

// Gets all of the required framework/service target tools for the specified service config
func (sm *serviceManager) GetRequiredTools(ctx context.Context, serviceConfig *ServiceConfig) ([]tools.ExternalTool, error) {
	frameworkService, err := sm.GetFrameworkService(ctx, serviceConfig)
	if err != nil {
		return nil, fmt.Errorf("getting framework service: %w", err)
	}

	serviceTarget, err := sm.GetServiceTarget(ctx, serviceConfig)
	if err != nil {
		return nil, fmt.Errorf("getting service target: %w", err)
	}

	requiredTools := []tools.ExternalTool{}
	requiredTools = append(requiredTools, frameworkService.RequiredExternalTools(ctx, serviceConfig)...)
	requiredTools = append(requiredTools, serviceTarget.RequiredExternalTools(ctx, serviceConfig)...)

	return tools.Unique(requiredTools), nil
}

// Initializes the service configuration and dependent framework & service target
// This allows frameworks & service targets to hook into a services lifecycle events
func (sm *serviceManager) Initialize(ctx context.Context, serviceConfig *ServiceConfig) error {
	frameworkService, err := sm.GetFrameworkService(ctx, serviceConfig)
	if err != nil {
		return fmt.Errorf("getting framework service: %w", err)
	}

	serviceTarget, err := sm.GetServiceTarget(ctx, serviceConfig)
	if err != nil {
		return fmt.Errorf("getting service target: %w", err)
	}

	if ok := sm.isComponentInitialized(serviceConfig, frameworkService); !ok {
		if err := frameworkService.Initialize(ctx, serviceConfig); err != nil {
			return err
		}

		sm.initialized[serviceConfig][frameworkService] = true
	}

	if ok := sm.isComponentInitialized(serviceConfig, serviceTarget); !ok {
		if err := serviceTarget.Initialize(ctx, serviceConfig); err != nil {
			return err
		}

		sm.initialized[serviceConfig][serviceTarget] = true
	}

	return nil
}

// Restores the code dependencies for the specified service config
func (sm *serviceManager) Restore(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	progress *async.Progress[ServiceProgress],
) (*ServiceRestoreResult, error) {
	cachedResult, ok := sm.getOperationResult(serviceConfig, string(ServiceEventRestore))
	if ok && cachedResult != nil {
		return cachedResult.(*ServiceRestoreResult), nil
	}

	frameworkService, err := sm.GetFrameworkService(ctx, serviceConfig)
	if err != nil {
		return nil, fmt.Errorf("getting framework services: %w", err)
	}

	restoreResult, err := runCommand(
		ctx,
		ServiceEventRestore,
		serviceConfig,
		func() (*ServiceRestoreResult, error) {
			return frameworkService.Restore(ctx, serviceConfig, progress)
		},
	)

	if err != nil {
		return nil, fmt.Errorf("failed restoring service '%s': %w", serviceConfig.Name, err)
	}

	sm.setOperationResult(serviceConfig, string(ServiceEventRestore), restoreResult)
	return restoreResult, nil
}

// Builds the code for the specified service config
// Will call the language compile for compiled languages or may copy build artifacts to a configured output folder
func (sm *serviceManager) Build(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	restoreOutput *ServiceRestoreResult,
	progress *async.Progress[ServiceProgress],
) (*ServiceBuildResult, error) {
	cachedResult, ok := sm.getOperationResult(serviceConfig, string(ServiceEventBuild))
	if ok && cachedResult != nil {
		return cachedResult.(*ServiceBuildResult), nil
	}

	if restoreOutput == nil {
		cachedResult, ok := sm.getOperationResult(serviceConfig, string(ServiceEventRestore))
		if ok && cachedResult != nil {
			restoreOutput = cachedResult.(*ServiceRestoreResult)
		}
	}

	frameworkService, err := sm.GetFrameworkService(ctx, serviceConfig)
	if err != nil {
		return nil, fmt.Errorf("getting framework services: %w", err)
	}

	buildResult, err := runCommand(
		ctx,
		ServiceEventBuild,
		serviceConfig,
		func() (*ServiceBuildResult, error) {
			return frameworkService.Build(ctx, serviceConfig, restoreOutput, progress)
		},
	)

	if err != nil {
		return nil, fmt.Errorf("failed building service '%s': %w", serviceConfig.Name, err)
	}

	sm.setOperationResult(serviceConfig, string(ServiceEventBuild), buildResult)
	return buildResult, nil
}

// Packages the code for the specified service config
// Depending on the service configuration this will generate an artifact that can be consumed by the hosting Azure service.
// Common examples could be a zip archive for app service or Docker images for container apps and AKS
func (sm *serviceManager) Package(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	buildOutput *ServiceBuildResult,
	progress *async.Progress[ServiceProgress],
	options *PackageOptions,
) (*ServicePackageResult, error) {
	if options == nil {
		options = &PackageOptions{}
	}

	cachedResult, ok := sm.getOperationResult(serviceConfig, string(ServiceEventPackage))
	if ok && cachedResult != nil {
		return cachedResult.(*ServicePackageResult), nil
	}

	if buildOutput == nil {
		cachedResult, ok := sm.getOperationResult(serviceConfig, string(ServiceEventBuild))
		if ok && cachedResult != nil {
			buildOutput = cachedResult.(*ServiceBuildResult)
		}
	}

	frameworkService, err := sm.GetFrameworkService(ctx, serviceConfig)
	if err != nil {
		return nil, fmt.Errorf("getting framework service: %w", err)
	}

	serviceTarget, err := sm.GetServiceTarget(ctx, serviceConfig)
	if err != nil {
		return nil, fmt.Errorf("getting service target: %w", err)
	}

	eventArgs := ServiceLifecycleEventArgs{
		Project: serviceConfig.Project,
		Service: serviceConfig,
	}

	hasBuildOutput := buildOutput != nil
	restoreResult := &ServiceRestoreResult{}

	// Get the language / framework requirements
	frameworkRequirements := frameworkService.Requirements()

	// When a previous restore result was not provided, and we require it
	// Then we need to restore the dependencies
	if frameworkRequirements.Package.RequireRestore && (!hasBuildOutput || buildOutput.Restore == nil) {
		restoreTaskResult, err := sm.Restore(ctx, serviceConfig, progress)
		if err != nil {
			return nil, err
		}

		restoreResult = restoreTaskResult
	}

	buildResult := &ServiceBuildResult{}

	// When a previous build result was not provided, and we require it
	// Then we need to build the project
	if frameworkRequirements.Package.RequireBuild && !hasBuildOutput {
		buildTaskResult, err := sm.Build(ctx, serviceConfig, restoreResult, progress)
		if err != nil {
			return nil, err
		}

		buildResult = buildTaskResult
	}

	if !hasBuildOutput {
		buildOutput = buildResult
		buildOutput.Restore = restoreResult
	}

	var packageResult *ServicePackageResult

	err = serviceConfig.Invoke(ctx, ServiceEventPackage, eventArgs, func() error {
		frameworkPackageResult, err := frameworkService.Package(ctx, serviceConfig, buildOutput, progress)
		if err != nil {
			return err
		}

		serviceTargetPackageResult, err := serviceTarget.Package(ctx, serviceConfig, frameworkPackageResult, progress)
		if err != nil {
			return err
		}

		packageResult = serviceTargetPackageResult
		sm.setOperationResult(serviceConfig, string(ServiceEventPackage), packageResult)

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed packaging service '%s': %w", serviceConfig.Name, err)
	}

	// Package path can be a file path or a container image name
	// We only move to desired output path for file based packages
	_, err = os.Stat(packageResult.PackagePath)
	hasPackageFile := err == nil

	if hasPackageFile && options.OutputPath != "" {
		var destFilePath string
		var destDirectory string

		isFilePath := filepath.Ext(options.OutputPath) != ""
		if isFilePath {
			destFilePath = options.OutputPath
			destDirectory = filepath.Dir(options.OutputPath)
		} else {
			destFilePath = filepath.Join(options.OutputPath, filepath.Base(packageResult.PackagePath))
			destDirectory = options.OutputPath
		}

		_, err := os.Stat(destDirectory)
		if errors.Is(err, os.ErrNotExist) {
			// Create the desired output directory if it does not already exist
			if err := os.MkdirAll(destDirectory, osutil.PermissionDirectory); err != nil {
				return nil, fmt.Errorf("failed creating output directory '%s': %w", destDirectory, err)
			}
		}

		// Move the package file to the desired path
		// We can't use os.Rename here since that does not work across disks
		if err := moveFile(packageResult.PackagePath, destFilePath); err != nil {
			return nil, fmt.Errorf(
				"failed moving package file '%s' to '%s': %w", packageResult.PackagePath, destFilePath, err)
		}

		packageResult.PackagePath = destFilePath
	}

	return packageResult, nil
}

// Deploys the generated artifacts to the Azure resource that will host the service application
// Common examples would be uploading zip archive using ZipDeploy deployment or
// pushing container images to a container registry.
func (sm *serviceManager) Deploy(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	packageResult *ServicePackageResult,
	progress *async.Progress[ServiceProgress],
) (*ServiceDeployResult, error) {
	cachedResult, ok := sm.getOperationResult(serviceConfig, string(ServiceEventDeploy))
	if ok && cachedResult != nil {
		return cachedResult.(*ServiceDeployResult), nil
	}

	if packageResult == nil {
		cachedResult, ok := sm.getOperationResult(serviceConfig, string(ServiceEventPackage))
		if ok && cachedResult != nil {
			packageResult = cachedResult.(*ServicePackageResult)
		}
	}

	serviceTarget, err := sm.GetServiceTarget(ctx, serviceConfig)
	if err != nil {
		return nil, fmt.Errorf("getting service target: %w", err)
	}

	var targetResource *environment.TargetResource

	if serviceConfig.Host == DotNetContainerAppTarget {
		containerEnvName := sm.env.GetServiceProperty(serviceConfig.Name, "CONTAINER_ENVIRONMENT_NAME")
		// AZURE_CONTAINER_APPS_ENVIRONMENT_ID is not required for Aspire (serviceConfig.DotNetContainerApp != nil)
		// because it uses a bicep deployment.
		if containerEnvName == "" && serviceConfig.DotNetContainerApp == nil {
			containerEnvName = sm.env.Getenv("AZURE_CONTAINER_APPS_ENVIRONMENT_ID")
			if containerEnvName == "" {
				return nil, fmt.Errorf(
					"could not determine container app environment for service %s, "+
						"have you set AZURE_CONTAINER_ENVIRONMENT_NAME or "+
						"SERVICE_%s_CONTAINER_ENVIRONMENT_NAME as an output of your "+
						"infrastructure?", serviceConfig.Name, strings.ToUpper(serviceConfig.Name))
			}

			parts := strings.Split(containerEnvName, "/")
			containerEnvName = parts[len(parts)-1]
		}

		// Get any explicitly configured resource group name
		// 1. Service level override
		// 2. Project level override
		resourceGroupNameTemplate := serviceConfig.ResourceGroupName
		if resourceGroupNameTemplate.Empty() {
			resourceGroupNameTemplate = serviceConfig.Project.ResourceGroupName
		}

		resourceGroupName, err := sm.resourceManager.GetResourceGroupName(
			ctx,
			sm.env.GetSubscriptionId(),
			resourceGroupNameTemplate,
		)
		if err != nil {
			return nil, fmt.Errorf("getting resource group name: %w", err)
		}

		targetResource = environment.NewTargetResource(
			sm.env.GetSubscriptionId(),
			resourceGroupName,
			containerEnvName,
			string(azapi.AzureResourceTypeContainerAppEnvironment),
		)
	} else {
		targetResource, err = sm.resourceManager.GetTargetResource(ctx, sm.env.GetSubscriptionId(), serviceConfig)
		if err != nil {
			return nil, fmt.Errorf("getting target resource: %w", err)
		}
	}

	deployResult, err := runCommand(
		ctx,
		ServiceEventDeploy,
		serviceConfig,
		func() (*ServiceDeployResult, error) {
			return serviceTarget.Deploy(ctx, serviceConfig, packageResult, targetResource, progress)
		},
	)

	if err != nil {
		return nil, fmt.Errorf("failed deploying service '%s': %w", serviceConfig.Name, err)
	}

	// Allow users to specify their own endpoints, in cases where they've configured their own front-end load balancers,
	// reverse proxies or DNS host names outside of the service target (and prefer that to be used instead).
	overriddenEndpoints := OverriddenEndpoints(ctx, serviceConfig, sm.env)
	if len(overriddenEndpoints) > 0 {
		deployResult.Endpoints = overriddenEndpoints
	}

	sm.setOperationResult(serviceConfig, string(ServiceEventDeploy), deployResult)
	return deployResult, nil
}

// GetServiceTarget constructs a ServiceTarget from the underlying service configuration
func (sm *serviceManager) GetServiceTarget(ctx context.Context, serviceConfig *ServiceConfig) (ServiceTarget, error) {
	var target ServiceTarget
	host := string(serviceConfig.Host)

	if alphaFeatureId, isAlphaFeature := alpha.IsFeatureKey(host); isAlphaFeature {
		if !sm.alphaFeatureManager.IsEnabled(alphaFeatureId) {
			return nil, fmt.Errorf(
				"service host '%s' is currently in alpha and needs to be enabled explicitly."+
					" Run `%s` to enable the feature.",
				host,
				alpha.GetEnableCommand(alphaFeatureId),
			)
		}
	}

	if err := sm.serviceLocator.ResolveNamed(host, &target); err != nil {
		return nil, fmt.Errorf(
			"failed to resolve service host '%s' for service '%s', %w",
			serviceConfig.Host,
			serviceConfig.Name,
			err,
		)
	}

	return target, nil
}

// GetFrameworkService constructs a framework service from the underlying service configuration
func (sm *serviceManager) GetFrameworkService(ctx context.Context, serviceConfig *ServiceConfig) (FrameworkService, error) {
	var frameworkService FrameworkService

	// Publishing from an existing image currently follows the same lifecycle as a docker project
	if serviceConfig.Language == ServiceLanguageNone && !serviceConfig.Image.Empty() {
		serviceConfig.Language = ServiceLanguageDocker
	}

	if err := sm.serviceLocator.ResolveNamed(string(serviceConfig.Language), &frameworkService); err != nil {
		return nil, fmt.Errorf(
			"failed to resolve language '%s' for service '%s', %w",
			serviceConfig.Language,
			serviceConfig.Name,
			err,
		)
	}

	var compositeFramework CompositeFrameworkService
	// For hosts which run in containers, if the source project is not already a container, we need to wrap it in a docker
	// project that handles the containerization.
	requiresLanguage := serviceConfig.Language != ServiceLanguageDocker && serviceConfig.Language != ServiceLanguageNone
	if serviceConfig.Host.RequiresContainer() && requiresLanguage {
		if err := sm.serviceLocator.ResolveNamed(string(ServiceLanguageDocker), &compositeFramework); err != nil {
			return nil, fmt.Errorf(
				"failed resolving composite framework service for '%s', language '%s': %w",
				serviceConfig.Name,
				serviceConfig.Language,
				err,
			)
		}
	} else if serviceConfig.Host == StaticWebAppTarget {
		withSwaConfig, err := swa.ContainsSwaConfig(serviceConfig.Path())
		if err != nil {
			return nil, fmt.Errorf("checking for swa-cli.config.json: %w", err)
		}
		if withSwaConfig {
			if err := sm.serviceLocator.ResolveNamed(string(ServiceLanguageSwa), &compositeFramework); err != nil {
				return nil, fmt.Errorf(
					"failed resolving composite framework service for '%s', language '%s': %w",
					serviceConfig.Name,
					serviceConfig.Language,
					err,
				)
			}
			log.Println("Using swa-cli for build and deploy because swa-cli.config.json was found in the service path")
		}
	}
	if compositeFramework != nil {
		compositeFramework.SetSource(frameworkService)
		frameworkService = compositeFramework
	}

	return frameworkService, nil
}

func OverriddenEndpoints(ctx context.Context, serviceConfig *ServiceConfig, env *environment.Environment) []string {
	overriddenEndpoints := env.GetServiceProperty(serviceConfig.Name, "ENDPOINTS")
	if overriddenEndpoints != "" {
		var endpoints []string
		err := json.Unmarshal([]byte(overriddenEndpoints), &endpoints)
		if err != nil {
			// This can only happen if the environment output was not a valid JSON array, which would be due to an authoring
			// error. For typical infra provider output passthrough, the infra provider would guarantee well-formed syntax
			log.Printf(
				"failed to unmarshal endpoints override for service '%s' as JSON array of strings: %v, skipping override",
				serviceConfig.Name,
				err)
		}

		return endpoints
	}

	return nil
}

// Attempts to retrieve the result of a previous operation from the cache
func (sm *serviceManager) getOperationResult(serviceConfig *ServiceConfig, operationName string) (any, bool) {
	key := fmt.Sprintf("%s:%s:%s", sm.env.Name(), serviceConfig.Name, operationName)
	value, ok := sm.operationCache[key]

	return value, ok
}

// Sets the result of an operation in the cache
func (sm *serviceManager) setOperationResult(serviceConfig *ServiceConfig, operationName string, result any) {
	key := fmt.Sprintf("%s:%s:%s", sm.env.Name(), serviceConfig.Name, operationName)
	sm.operationCache[key] = result
}

// isComponentInitialized Checks if a component has been initialized for a service configuration
func (sm *serviceManager) isComponentInitialized(serviceConfig *ServiceConfig, component any) bool {
	if componentMap, has := sm.initialized[serviceConfig]; has && len(componentMap) > 0 {
		initialized := false
		if ok, has := componentMap[component]; has && ok {
			initialized = ok
		}

		return initialized
	}

	sm.initialized[serviceConfig] = map[any]bool{}

	return false
}

func runCommand[T any](
	ctx context.Context,
	eventName ext.Event,
	serviceConfig *ServiceConfig,
	fn func() (T, error),
) (T, error) {
	eventArgs := ServiceLifecycleEventArgs{
		Project: serviceConfig.Project,
		Service: serviceConfig,
	}

	var result T

	err := serviceConfig.Invoke(ctx, eventName, eventArgs, func() error {
		res, err := fn()
		result = res
		return err
	})

	return result, err
}

// Copies a file from the source path to the destination path
// Deletes the source file after the copy is complete
func moveFile(sourcePath string, destinationPath string) error {
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("opening source file: %w", err)
	}
	defer sourceFile.Close()

	// Create or truncate the destination file
	destinationFile, err := os.Create(destinationPath)
	if err != nil {
		return fmt.Errorf("creating destination file: %w", err)
	}
	defer destinationFile.Close()

	// Copy the contents of the source file to the destination file
	_, err = io.Copy(destinationFile, sourceFile)
	if err != nil {
		return fmt.Errorf("copying file: %w", err)
	}

	// Remove the source file (optional)
	defer os.Remove(sourcePath)

	return nil
}

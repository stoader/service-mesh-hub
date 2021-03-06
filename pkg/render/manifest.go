package render

import (
	"bytes"
	"context"
	"text/template"

	"github.com/solo-io/service-mesh-hub/pkg/render/validation"
	"github.com/solo-io/solo-kit/pkg/api/v1/resources/core"

	errors "github.com/rotisserie/eris"
	"github.com/solo-io/go-utils/contextutils"
	"github.com/solo-io/go-utils/installutils"
	"github.com/solo-io/go-utils/installutils/helmchart"
	"github.com/solo-io/go-utils/installutils/kuberesource"
	hubv1 "github.com/solo-io/service-mesh-hub/api/v1"
	"go.uber.org/zap"
)

var (
	MissingInstallSpecError = errors.Errorf("missing installation spec")

	FailedToRenderManifestsError = func(err error) error {
		return errors.Wrapf(err, "error rendering manifests")
	}

	FailedToConvertManifestsError = func(err error) error {
		return errors.Wrapf(err, "error converting manifests to raw resources")
	}

	FailedRenderValueTemplatesError = func(err error) error {
		return errors.Wrapf(err, "error rendering input value templates")
	}

	MissingInputForRequiredLayer = func(err error) error {
		return errors.Wrapf(err, "error retrieving input for required layer")
	}

	MissingInputForRequireParam = func(name string) error {
		return errors.Errorf("Missing input for required parameter %v", name)
	}

	UnrecognizedParamError = func(name string) error {
		return errors.Errorf("Parameter %v is not specified on the selected versioned application spec, flavor, or layer option", name)
	}

	IncorrectNumberOfInputLayersError = errors.Errorf("incorrect number of input layers")
)

type SuperglooInfo struct {
	Namespace          string
	ServiceAccountName string
	ClusterRoleName    string
}

type LayerInput struct {
	LayerId, OptionId string
}

type ValuesInputs struct {
	Name             string
	InstallNamespace string
	Flavor           *hubv1.Flavor
	Layers           []LayerInput
	MeshRef          core.ResourceRef

	UserDefinedValues string
	SpecDefinedValues string
	// These map to the params found on versions, flavors, and layers,
	Params map[string]string
}

// Deprecated: use ManifestRenderer.ComputeResourcesForApplication
func ComputeResourcesForApplication(ctx context.Context, inputs ValuesInputs, spec *hubv1.VersionedApplicationSpec) (kuberesource.UnstructuredResources, error) {
	renderer := NewManifestRenderer(validation.NoopValidateResources)
	return renderer.ComputeResourcesForApplication(ctx, inputs, spec)
}

func ValidateInputs(inputs ValuesInputs, spec hubv1.VersionedApplicationSpec, validate validation.ValidateResourceDependencies) error {
	// Validate layers and layer options.
	if len(inputs.Layers) < GetRequiredLayerCount(inputs.Flavor) {
		return IncorrectNumberOfInputLayersError
	}

	var selectedOptions []*hubv1.LayerOption
	for _, flavorLayer := range inputs.Flavor.CustomizationLayers {
		var optionId string
		for _, layerInput := range inputs.Layers {
			if layerInput.LayerId == flavorLayer.Id {
				optionId = layerInput.OptionId
			}
		}

		option, err := GetLayerOption(optionId, flavorLayer)
		if err != nil && !flavorLayer.Optional {
			return MissingInputForRequiredLayer(err)
		}
		if option != nil {
			selectedOptions = append(selectedOptions, option)
		}
	}

	for _, o := range selectedOptions {
		if err := validate(o.GetResourceDependencies()); err != nil {
			return err
		}
	}

	// Validate parameters.
	allParameters := make(map[string]*hubv1.Parameter)
	for _, param := range spec.GetParameters() {
		allParameters[param.Name] = param
	}
	for _, param := range inputs.Flavor.GetParameters() {
		allParameters[param.Name] = param
	}
	for _, option := range selectedOptions {
		for _, param := range option.Parameters {
			allParameters[param.Name] = param
		}
	}
	for _, param := range allParameters {
		if value := inputs.Params[param.Name]; param.Required && value == "" {
			return MissingInputForRequireParam(param.Name)
		}
	}
	for name := range inputs.Params {
		if _, ok := allParameters[name]; !ok {
			return UnrecognizedParamError(name)
		}
	}

	return nil
}

/*
 Coalesces spec values yaml, layer values, params, and user-defined values yaml.
 User defined values override params which override layer values which override spec values.
 If there is an error parsing, it is logged and propagated.
*/
func ComputeValueOverrides(ctx context.Context, inputs ValuesInputs) (string, error) {
	valuesMap := make(map[string]interface{})

	specValues, err := ConvertYamlStringToNestedMap(inputs.SpecDefinedValues)
	if err != nil {
		contextutils.LoggerFrom(ctx).Errorw("Error parsing spec values yaml",
			zap.Error(err),
			zap.String("values", inputs.SpecDefinedValues))
		return "", err
	}
	valuesMap = CoalesceValuesMap(ctx, valuesMap, specValues)

	for _, layerInput := range inputs.Layers {
		option, err := GetLayerOptionFromFlavor(layerInput.LayerId, layerInput.OptionId, inputs.Flavor)
		if err != nil {
			return "", err
		}

		if option.HelmValues != "" {
			layerValues, err := ConvertYamlStringToNestedMap(option.HelmValues)
			if err != nil {
				contextutils.LoggerFrom(ctx).Errorw("Error parsing layer values yaml",
					zap.Error(err),
					zap.String("values", option.HelmValues))
				return "", err
			}
			valuesMap = CoalesceValuesMap(ctx, valuesMap, layerValues)
		}
	}

	paramValues, err := ConvertParamsToNestedMap(inputs.Params)
	if err != nil {
		contextutils.LoggerFrom(ctx).Errorw("Error parsing install params",
			zap.Error(err))
		return "", err
	}
	valuesMap = CoalesceValuesMap(ctx, valuesMap, paramValues)

	userValues, err := ConvertYamlStringToNestedMap(inputs.UserDefinedValues)
	if err != nil {
		contextutils.LoggerFrom(ctx).Errorw("Error parsing user values yaml",
			zap.Error(err),
			zap.Any("params", inputs.UserDefinedValues))
		return "", err
	}
	valuesMap = CoalesceValuesMap(ctx, valuesMap, userValues)

	values, err := ConvertNestedMapToYaml(valuesMap)
	if err != nil {
		contextutils.LoggerFrom(ctx).Errorw(err.Error(), zap.Error(err), zap.Any("valuesMap", valuesMap))
		return "", err
	}
	return values, nil
}

func GetManifestsFromApplicationSpec(ctx context.Context, inputs ValuesInputs, spec *hubv1.VersionedApplicationSpec) (helmchart.Manifests, error) {
	var manifests helmchart.Manifests
	switch installationSpec := spec.GetInstallationSpec().(type) {
	case *hubv1.VersionedApplicationSpec_GithubChart:
		githubManifests, err := getManifestsFromGithub(ctx, installationSpec.GithubChart, inputs)
		if err != nil {
			return nil, err
		}
		manifests = githubManifests
	case *hubv1.VersionedApplicationSpec_HelmArchive:
		helmManifests, err := getManifestsFromHelm(ctx, installationSpec.HelmArchive, inputs)
		if err != nil {
			return nil, err
		}
		manifests = helmManifests
	case *hubv1.VersionedApplicationSpec_ManifestsArchive:
		archiveManifests, err := getManifestsFromArchive(ctx, installationSpec.ManifestsArchive, inputs)
		if err != nil {
			return nil, err
		}
		manifests = archiveManifests
	case *hubv1.VersionedApplicationSpec_InstallationSteps:
		archiveManifests, err := getManifestsFromSteps(ctx, installationSpec.InstallationSteps, inputs)
		if err != nil {
			return nil, err
		}
		manifests = archiveManifests
	default:
		return nil, MissingInstallSpecError
	}

	return manifests, nil
}

func FilterByLabel(ctx context.Context, spec *hubv1.VersionedApplicationSpec, resources kuberesource.UnstructuredResources) kuberesource.UnstructuredResources {
	labels := spec.GetRequiredLabels()
	if len(labels) > 0 {
		contextutils.LoggerFrom(ctx).Infow("Filtering installed resources by label", zap.Any("labels", labels))
		return resources.WithLabels(labels)
	}
	return resources
}

func getManifestsFromHelm(ctx context.Context, helmInstallSpec *hubv1.TgzLocation, inputs ValuesInputs) (helmchart.Manifests, error) {
	values, err := ComputeValueOverrides(ctx, inputs)
	if err != nil {
		return nil, err
	}
	contextutils.LoggerFrom(ctx).Infow("Rendering with values", zap.String("values", values))
	manifests, err := helmchart.RenderManifests(ctx,
		helmInstallSpec.Uri,
		values,
		inputs.Name,
		inputs.InstallNamespace,
		"")
	if err != nil {
		wrapped := FailedToRenderManifestsError(err)
		contextutils.LoggerFrom(ctx).Errorw(wrapped.Error(),
			zap.Error(err),
			zap.String("chartUri", helmInstallSpec.Uri),
			zap.String("values", values),
			zap.String("releaseName", inputs.Name),
			zap.String("namespace", inputs.InstallNamespace),
			zap.String("kubeVersion", ""))
		return nil, wrapped
	}
	return manifests, nil
}

func getManifestsFromGithub(ctx context.Context, githubInstallSpec *hubv1.GithubRepositoryLocation, inputs ValuesInputs) (helmchart.Manifests, error) {
	ref := helmchart.GithubChartRef{
		Owner:          githubInstallSpec.Org,
		Repo:           githubInstallSpec.Repo,
		Ref:            githubInstallSpec.Ref,
		ChartDirectory: githubInstallSpec.Directory,
	}
	values, err := ComputeValueOverrides(ctx, inputs)
	if err != nil {
		return nil, err
	}
	manifests, err := helmchart.RenderManifestsFromGithub(ctx, ref,
		values,
		inputs.Name,
		inputs.InstallNamespace,
		"")
	if err != nil {
		wrapped := FailedToRenderManifestsError(err)
		contextutils.LoggerFrom(ctx).Errorw(wrapped.Error(),
			zap.Error(err),
			zap.Any("ref", ref),
			zap.String("values", values),
			zap.String("releaseName", inputs.Name),
			zap.String("namespace", inputs.InstallNamespace),
			zap.String("kubeVersion", ""))
		return nil, wrapped
	}
	return manifests, nil
}

func getManifestsFromArchive(ctx context.Context, manifestsArchive *hubv1.TgzLocation, inputs ValuesInputs) (helmchart.Manifests, error) {
	manifests, err := installutils.GetManifestsFromRemoteTar(manifestsArchive.GetUri())
	if err != nil {
		wrapped := FailedToRenderManifestsError(err)
		contextutils.LoggerFrom(ctx).Errorw(wrapped.Error(),
			zap.Error(err),
			zap.String("manifestsArchiveUrl", manifestsArchive.GetUri()),
			zap.String("releaseName", inputs.Name),
			zap.String("namespace", inputs.InstallNamespace))
		return nil, wrapped
	}
	return manifests, nil
}

const InstallationStepLabel = "service-mesh-hub.solo.io/installation_step"

func getManifestsFromSteps(ctx context.Context, steps *hubv1.InstallationSteps, inputs ValuesInputs) (helmchart.Manifests, error) {
	if len(steps.Steps) == 0 {
		return nil, errors.Errorf("must provide at least one installation step")
	}
	var combinedManifests helmchart.Manifests
	var uniqueStepNames []string
	for _, step := range steps.Steps {
		if step.Name == "" {
			return nil, errors.Errorf("step must be named")
		}
		for _, name := range uniqueStepNames {
			if step.Name == name {
				return nil, errors.Errorf("step names must be unique; %v duplicated", name)
			}
		}
		uniqueStepNames = append(uniqueStepNames, step.Name)

		manifests, err := getManifestsFromInstallationStep(ctx, inputs, step)
		if err != nil {
			return nil, err
		}
		// add labels for step to every resource in the manifests
		resources, err := manifests.ResourceList()
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			labels := resource.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}
			labels[InstallationStepLabel] = step.Name
			resource.SetLabels(labels)
		}

		manifests, err = helmchart.ManifestsFromResources(resources)
		if err != nil {
			return nil, err
		}

		combinedManifests = append(combinedManifests, manifests...)
	}
	return combinedManifests, nil
}

func getManifestsFromInstallationStep(ctx context.Context, inputs ValuesInputs, step *hubv1.InstallationSteps_Step) (helmchart.Manifests, error) {
	var manifests helmchart.Manifests
	switch installationSpec := step.Step.(type) {
	case *hubv1.InstallationSteps_Step_GithubChart:
		githubManifests, err := getManifestsFromGithub(ctx, installationSpec.GithubChart, inputs)
		if err != nil {
			return nil, err
		}
		manifests = githubManifests
	case *hubv1.InstallationSteps_Step_HelmArchive:
		helmManifests, err := getManifestsFromHelm(ctx, installationSpec.HelmArchive, inputs)
		if err != nil {
			return nil, err
		}
		manifests = helmManifests
	case *hubv1.InstallationSteps_Step_ManifestsArchive:
		archiveManifests, err := getManifestsFromArchive(ctx, installationSpec.ManifestsArchive, inputs)
		if err != nil {
			return nil, err
		}
		manifests = archiveManifests
	default:
		return nil, MissingInstallSpecError
	}

	return manifests, nil
}

// The SpecDefinedValues, UserDefinedValues, and Params inputs can contain template
// actions (text delimited by "{{" and "}}" ). This function renders the contents of these
// parameters using the data contained in 'input' and updates 'input' with the results.
func ExecInputValuesTemplates(inputs ValuesInputs) (ValuesInputs, error) {

	// Render the helm values string that comes from the extension spec
	buf := new(bytes.Buffer)
	tpl := template.Must(template.New("specValues").Parse(inputs.SpecDefinedValues))
	if err := tpl.Execute(buf, inputs); err != nil {
		return ValuesInputs{}, err
	}
	inputs.SpecDefinedValues = buf.String()
	buf.Reset()

	// Render the helm values string that comes from the user provided overrides
	tpl = template.Must(template.New("userValues").Parse(inputs.UserDefinedValues))
	if err := tpl.Execute(buf, inputs); err != nil {
		return ValuesInputs{}, err
	}
	inputs.UserDefinedValues = buf.String()
	buf.Reset()

	// Render the values of the parameters
	for paramName, paramValue := range inputs.Params {
		t := template.Must(template.New(paramName).Parse(paramValue))
		if err := t.Execute(buf, inputs); err != nil {
			return ValuesInputs{}, err
		}
		inputs.Params[paramName] = buf.String()
		buf.Reset()
	}

	return inputs, nil
}

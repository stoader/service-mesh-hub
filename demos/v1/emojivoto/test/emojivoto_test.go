package test

import (
	"context"
	"github.com/solo-io/service-mesh-hub/pkg/render/validation"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/solo-io/go-utils/manifesttestutils"
	v1 "github.com/solo-io/service-mesh-hub/api/v1"
	"github.com/solo-io/service-mesh-hub/pkg/render"
	"github.com/solo-io/service-mesh-hub/test"
	"github.com/solo-io/solo-kit/pkg/api/v1/resources/core"
)

var _ = Describe("emojivoto", func() {

	const (
		namespace = "app"
		name      = "emojivoto"
		meshName  = "linkerd"
	)

	var (
		spec         *v1.ApplicationSpec
		versionMap   map[string]*v1.VersionedApplicationSpec
		version      *v1.VersionedApplicationSpec
		inputs       render.ValuesInputs
		testManifest TestManifest
	)

	BeforeEach(func() {
		spec = test.LoadApplicationSpec("../spec.yaml")
		versionMap = make(map[string]*v1.VersionedApplicationSpec)
		for _, version := range spec.Versions {
			versionMap[version.Version] = version
		}
		inputs = render.ValuesInputs{
			Name:             name,
			InstallNamespace: namespace,
			MeshRef: core.ResourceRef{
				Name:      meshName,
				Namespace: namespace,
			},
		}
	})

	bindVersion := func(versionString string) {
		version = versionMap[versionString]
		inputs.SpecDefinedValues = version.ValuesYaml
		inputs.Flavor = test.GetFlavor("default", versionMap[versionString])
		renderer := render.NewManifestRenderer(validation.NoopValidateResources)
		rendered, err := renderer.ComputeResourcesForApplication(context.TODO(), inputs, version)
		Expect(err).NotTo(HaveOccurred())
		testManifest = NewTestManifestWithResources(rendered)
	}

	Context("latest with default values", func() {
		BeforeEach(func() {
			bindVersion("latest")
		})

		It("has the correct number of resources", func() {
			Expect(testManifest.NumResources()).To(Equal(11))
		})
	})
})
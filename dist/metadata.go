package dist

const BuildpackLayersLabel = "io.buildpacks.buildpack.layers"

type BuildpackLayers map[string]map[string]BuildpackLayerInfo

type BuildpackLayerInfo struct {
	LayerDigest string `json:"layerDigest"`
	LayerDiffID string `json:"layerDiffID"`
	Order       Order  `json:"order,omitempty"`
}

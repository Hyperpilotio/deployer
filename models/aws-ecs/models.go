package deployer

// Convered using https://mholt.github.io/json-to-go/
type TaskDefinition struct {
	ContainerDefinitions []struct {
		Name        string   `json:"name"`
		Links       []string `json:"links,omitempty"`
		Image       string   `json:"image"`
		Essential   bool     `json:"essential"`
		MountPoints []struct {
			SourceVolume  string `json:"sourceVolume"`
			ContainerPath string `json:"containerPath"`
		} `json:"mountPoints,omitempty"`
		PortMappings []struct {
			ContainerPort int `json:"containerPort"`
			HostPort      int `json:"hostPort"`
		} `json:"portMappings,omitempty"`
		Memory      int `json:"memory"`
		CPU         int `json:"cpu"`
		Environment []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"environment,omitempty"`
	} `json:"containerDefinitions"`
	Volumes []struct {
		Name string `json:"name"`
		Host struct {
			SourcePath string `json:"sourcePath"`
		} `json:"host"`
	} `json:"volumes"`
	Family string `json:"family"`
}

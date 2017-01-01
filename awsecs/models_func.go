package awsecs

import "fmt"

// Service return a string with suffix "-service"
func (mapping NodeMapping) Service() string {
	return mapping.Task + "-service"
}

// ImageIdAttribute return imageId for putAttribute function
func (mapping NodeMapping) ImageIdAttribute() string {
	return fmt.Sprintf("imageId-%d", mapping.Id)
}

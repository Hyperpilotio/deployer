package share

// ServiceAddress object that stores the information of service container
type ServiceAddress struct {
	Host string `bson:"host,omitempty" json:"host,omitempty"`
	Port int32  `bson:"port,omitempty" json:"port,omitempty"`
}

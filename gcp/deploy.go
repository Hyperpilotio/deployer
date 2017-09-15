package gcp

import (
	"errors"
	"io/ioutil"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type GCPProfile struct {
	UserId             string
	ProjectId          string
	Scopes             []string
	ServiceAccountPath string
}

type NodeInfo struct {
}

// GCPCluster stores the data of a google cloud platform backed cluster
type GCPCluster struct {
	Zone           string
	Name           string
	ClusterVersion string
	GCPProfile     *GCPProfile
	NodeInfos      map[int]*NodeInfo
}

func NewGCPCluster(zone string, name string, clusterVersion string, gcpProfile *GCPProfile) *GCPCluster {
	return &GCPCluster{
		Zone:           zone,
		Name:           name,
		ClusterVersion: clusterVersion,
		GCPProfile:     gcpProfile,
		NodeInfos:      make(map[int]*NodeInfo),
	}
}

func CreateClient(gcpProfile *GCPProfile, Zone string) (*http.Client, error) {
	dat, err := ioutil.ReadFile(gcpProfile.ServiceAccountPath)
	if err != nil {
		return nil, errors.New("Unable to read service account file: " + err.Error())
	}

	conf, err := google.JWTConfigFromJSON(dat, gcpProfile.Scopes...)
	if err != nil {
		return nil, errors.New("Unable to acquire generate config: " + err.Error())
	}

	return conf.Client(oauth2.NoContext), nil
}

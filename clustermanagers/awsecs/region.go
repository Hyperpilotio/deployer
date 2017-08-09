package awsecs

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"sort"
	"strings"
	"time"
)

var awsRegionPricingApiUrl = "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/$REGION/index.json"

type Attributes struct {
	Servicecode                 string `json:"servicecode"`
	Location                    string `json:"location"`
	LocationType                string `json:"locationType"`
	InstanceType                string `json:"instanceType"`
	CurrentGeneration           string `json:"currentGeneration"`
	InstanceFamily              string `json:"instanceFamily"`
	Vcpu                        string `json:"vcpu"`
	PhysicalProcessor           string `json:"physicalProcessor"`
	ClockSpeed                  string `json:"clockSpeed"`
	Memory                      string `json:"memory"`
	Storage                     string `json:"storage"`
	NetworkPerformance          string `json:"networkPerformance"`
	ProcessorArchitecture       string `json:"processorArchitecture"`
	Tenancy                     string `json:"tenancy"`
	OperatingSystem             string `json:"operatingSystem"`
	LicenseModel                string `json:"licenseModel"`
	Usagetype                   string `json:"usagetype"`
	Operation                   string `json:"operation"`
	DedicatedEbsThroughput      string `json:"dedicatedEbsThroughput"`
	Ecu                         string `json:"ecu"`
	EnhancedNetworkingSupported string `json:"enhancedNetworkingSupported"`
	NormalizationSizeFactor     string `json:"normalizationSizeFactor"`
	PreInstalledSw              string `json:"preInstalledSw"`
	ProcessorFeatures           string `json:"processorFeatures"`
}

type Product struct {
	Sku           string     `json:"sku"`
	ProductFamily string     `json:"productFamily"`
	Attributes    Attributes `json:"attributes"`
}

type RegionProducts struct {
	FormatVersion   string             `json:"formatVersion"`
	Disclaimer      string             `json:"disclaimer"`
	OfferCode       string             `json:"offerCode"`
	Version         string             `json:"version"`
	PublicationDate time.Time          `json:"publicationDate"`
	Products        map[string]Product `json:"products"`
}

func GetSupportInstances(region string) ([]string, error) {
	requestUrl := strings.Replace(awsRegionPricingApiUrl, "$REGION", region, 1)
	resp, err := http.Get(requestUrl)
	if err != nil {
		return nil, errors.New("Unable to send request to aws pricing api: " + err.Error())
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.New("Unable to get response from aws pricing api: " + err.Error())
	}

	regionProducts := &RegionProducts{}
	if err := json.Unmarshal(body, &regionProducts); err != nil {
		return nil, errors.New("Unable to parse aws pricing api response: " + err.Error())
	}

	instanceTypes := []string{}
	for _, product := range regionProducts.Products {
		if product.Attributes.CurrentGeneration == "Yes" {
			if product.ProductFamily == "Compute Instance" && product.Attributes.Vcpu != "" {
				if !inArray(product.Attributes.InstanceType, instanceTypes) {
					instanceTypes = append(instanceTypes, product.Attributes.InstanceType)
				}
			}
		}
	}

	sort.Strings(instanceTypes)
	return instanceTypes, nil
}

func inArray(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

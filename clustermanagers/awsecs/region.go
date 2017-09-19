package awsecs

import (
	"fmt"
	"sort"

	hpaws "github.com/hyperpilotio/deployer/clusters/aws"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
)

func GetSupportInstanceTypes(
	awsProfile *hpaws.AWSProfile,
	regionName string,
	availabilityZoneName string) ([]string, error) {
	sess, sessionErr := hpaws.CreateSession(awsProfile, regionName)
	if sessionErr != nil {
		glog.Errorf("Unable to create session: %s" + sessionErr.Error())
		return nil, sessionErr
	}

	ec2Svc := ec2.New(sess)

	azResp, err := ec2Svc.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("region-name"),
				Values: []*string{
					aws.String(regionName),
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("Unable to describe availability zones: %s", err.Error())
	}

	zoneNames := []string{}
	for _, az := range azResp.AvailabilityZones {
		if aws.StringValue(az.State) == "available" {
			zoneNames = append(zoneNames, aws.StringValue(az.ZoneName))
		}
	}

	if !inArray(availabilityZoneName, zoneNames) {
		return nil, fmt.Errorf("Unsupported %s availabilityZone in %s region: ", availabilityZoneName, regionName)
	}

	index := 0
	supportInstanceTypes := []string{}
	var nextToken *string
	for nextToken != nil || index == 0 {
		describeReservedInstancesOfferingsInput := &ec2.DescribeReservedInstancesOfferingsInput{
			AvailabilityZone: aws.String(availabilityZoneName),
		}

		if index != 0 {
			describeReservedInstancesOfferingsInput.NextToken = nextToken
		}

		resp, err := ec2Svc.DescribeReservedInstancesOfferings(describeReservedInstancesOfferingsInput)
		if err != nil {
			return nil, fmt.Errorf("Unable to describe reserved instances offerings: %s", err.Error())
		}

		for _, reservedInstancesOffering := range resp.ReservedInstancesOfferings {
			instanceType := aws.StringValue(reservedInstancesOffering.InstanceType)
			if !inArray(instanceType, supportInstanceTypes) {
				supportInstanceTypes = append(supportInstanceTypes, instanceType)
			}
		}
		nextToken = resp.NextToken
		index++
	}

	sort.Strings(supportInstanceTypes)
	return supportInstanceTypes, nil
}

func inArray(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

package awsecs

import (
	"fmt"
	"sort"

	hpaws "github.com/hyperpilotio/deployer/aws"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
)

func GetSupportInstanceTypes(awsProfile *hpaws.AWSProfile, region string, availabilityZone string) ([]string, error) {
	sess, sessionErr := hpaws.CreateSession(awsProfile, region)
	if sessionErr != nil {
		glog.Errorf("Unable to create session: %s" + sessionErr.Error())
		return nil, sessionErr
	}

	ec2Svc := ec2.New(sess)

	resp, err := ec2Svc.DescribeReservedInstancesOfferings(&ec2.DescribeReservedInstancesOfferingsInput{
		AvailabilityZone: aws.String(availabilityZone),
	})
	if err != nil {
		return nil, fmt.Errorf("Unable to describe reserved instances offerings: %s", err.Error())
	}

	supportInstanceTypes := []string{}
	for _, reservedInstancesOffering := range resp.ReservedInstancesOfferings {
		instanceType := aws.StringValue(reservedInstancesOffering.InstanceType)
		if !inArray(instanceType, supportInstanceTypes) {
			supportInstanceTypes = append(supportInstanceTypes, instanceType)
		}
	}
	if err := recursiveSetInstanceTypes(ec2Svc, availabilityZone,
		resp.NextToken, &supportInstanceTypes); err != nil {
		return nil, fmt.Errorf("Unable to get support instance types: %s", err.Error())
	}

	sort.Strings(supportInstanceTypes)
	return supportInstanceTypes, nil
}

func recursiveSetInstanceTypes(
	ec2Svc *ec2.EC2,
	availabilityZone string,
	nextToken *string,
	supportInstanceTypes *[]string) error {
	if nextToken == nil {
		return nil
	}

	resp, err := ec2Svc.DescribeReservedInstancesOfferings(&ec2.DescribeReservedInstancesOfferingsInput{
		AvailabilityZone: aws.String(availabilityZone),
		NextToken:        nextToken,
	})
	if err != nil {
		return fmt.Errorf("Unable to describe reserved instances offerings: %s", err.Error())
	}

	for _, reservedInstancesOffering := range resp.ReservedInstancesOfferings {
		instanceType := aws.StringValue(reservedInstancesOffering.InstanceType)
		if !inArray(instanceType, *supportInstanceTypes) {
			*supportInstanceTypes = append(*supportInstanceTypes, instanceType)
		}
	}

	if err := recursiveSetInstanceTypes(ec2Svc, availabilityZone,
		resp.NextToken, supportInstanceTypes); err != nil {
		return fmt.Errorf("Unable to recursive set instances types: %s", err.Error())
	}

	return nil
}

func inArray(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

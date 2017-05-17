package store

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/simpledb"
	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/spf13/viper"
)

type DomainType int

// Possible simpledb domain type
const (
	DEPLOYMENT = 0
	AWSPROFILE = 1
)

func getTypeString(config *viper.Viper, domainType DomainType) string {
	switch domainType {
	case DEPLOYMENT:
		return config.GetString("store.deploymentDomain")
	case AWSPROFILE:
		return config.GetString("store.awsProfileDomain")
	}

	return ""
}

type SimpleDB struct {
	Region string
	Config *viper.Viper
	Sess   *session.Session
}

func NewSimpleDB(config *viper.Viper) (*SimpleDB, error) {
	region := strings.ToLower(config.GetString("store.region"))
	session, err := createSessionByRegion(config, region)
	if err != nil {
		return nil, err
	}

	domainTypes := []DomainType{DEPLOYMENT, AWSPROFILE}
	if err := createDomains(session, config, domainTypes); err != nil {
		return nil, err
	}

	return &SimpleDB{
		Region: region,
		Config: config,
		Sess:   session,
	}, nil
}

func createDomains(sess *session.Session, config *viper.Viper, domainTypes []DomainType) error {
	simpledbSvc := simpledb.New(sess)
	for _, domainType := range domainTypes {
		createDomainInput := &simpledb.CreateDomainInput{
			DomainName: aws.String(getTypeString(config, domainType)),
		}

		if _, err := simpledbSvc.CreateDomain(createDomainInput); err != nil {
			return errors.New("Unable to create simpleDB domain: " + err.Error())
		}
	}

	return nil
}

func (db *SimpleDB) StoreNewDeployment(deployment *awsecs.StoreDeployment) error {
	domainName := getTypeString(db.Config, DEPLOYMENT)
	simpledbSvc := simpledb.New(db.Sess)

	attributes := []*simpledb.ReplaceableAttribute{}
	recursiveStructField(&attributes, deployment)

	putAttributesInput := &simpledb.PutAttributesInput{
		Attributes: attributes,
		DomainName: aws.String(domainName),
		ItemName:   aws.String(deployment.Name),
	}

	if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
		return errors.New("Unable to put attributes to simpleDB: " + err.Error())
	}

	return nil
}

func (db *SimpleDB) LoadDeployments() ([]*awsecs.StoreDeployment, error) {
	domainName := getTypeString(db.Config, DEPLOYMENT)
	simpledbSvc := simpledb.New(db.Sess)

	selectExpression := fmt.Sprintf("select * from `%s`", domainName)
	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	deployments := []*awsecs.StoreDeployment{}
	selectOutput, err := simpledbSvc.Select(selectInput)
	if err != nil {
		return nil, errors.New("Unable to select data from simpleDB: " + err.Error())
	}

	for _, item := range selectOutput.Items {
		getAttributesInput := &simpledb.GetAttributesInput{
			DomainName: aws.String(domainName),
			ItemName:   item.Name,
		}

		resp, err := simpledbSvc.GetAttributes(getAttributesInput)
		if err != nil {
			return nil, errors.New("Unable to get attributes from simpleDB: " + err.Error())
		}

		deployment := &awsecs.StoreDeployment{
			K8SDeployment: &awsecs.K8SStoreDeployment{},
		}
		recursiveSetValue(deployment, resp.Attributes)

		deployments = append(deployments, deployment)
	}

	return deployments, nil
}

func (db *SimpleDB) DeleteDeployment(deploymentName string) error {
	domainName := getTypeString(db.Config, DEPLOYMENT)
	simpledbSvc := simpledb.New(db.Sess)

	selectExpression := fmt.Sprintf("select * from `%s` where itemName()='%s'", domainName, deploymentName)
	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	selectOutput, err := simpledbSvc.Select(selectInput)
	if err != nil {
		return errors.New("Unable to select data from simpleDB: " + err.Error())
	}

	if len(selectOutput.Items) == 0 {
		return fmt.Errorf("Unable to find %s data from simpleDB...", deploymentName)
	}

	deleteAttributesInput := &simpledb.DeleteAttributesInput{
		DomainName: aws.String(domainName),
		ItemName:   selectOutput.Items[0].Name,
	}

	if _, err := simpledbSvc.DeleteAttributes(deleteAttributesInput); err != nil {
		return fmt.Errorf("Unable to delete %s attributes from simpleDB: %s", deploymentName, err.Error())
	}

	return nil
}

func (db *SimpleDB) StoreNewAWSProfile(awsProfile *awsecs.AWSProfile) error {
	domainName := getTypeString(db.Config, AWSPROFILE)
	simpledbSvc := simpledb.New(db.Sess)

	attributes := []*simpledb.ReplaceableAttribute{}
	recursiveStructField(&attributes, awsProfile)

	putAttributesInput := &simpledb.PutAttributesInput{
		Attributes: attributes,
		DomainName: aws.String(domainName),
		ItemName:   aws.String(awsProfile.UserId),
	}

	if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
		return errors.New("Unable to put attributes to simpleDB: " + err.Error())
	}

	return nil
}

func (db *SimpleDB) LoadAWSProfiles() ([]*awsecs.AWSProfile, error) {
	domainName := getTypeString(db.Config, AWSPROFILE)
	simpledbSvc := simpledb.New(db.Sess)

	selectExpression := fmt.Sprintf("select * from `%s`", domainName)
	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	awsProfiles := []*awsecs.AWSProfile{}
	selectOutput, err := simpledbSvc.Select(selectInput)
	if err != nil {
		return nil, errors.New("Unable to select data from simpleDB: " + err.Error())
	}

	for _, item := range selectOutput.Items {
		getAttributesInput := &simpledb.GetAttributesInput{
			DomainName: aws.String(domainName),
			ItemName:   item.Name,
		}

		resp, err := simpledbSvc.GetAttributes(getAttributesInput)
		if err != nil {
			return nil, errors.New("Unable to get attributes from simpleDB: " + err.Error())
		}

		awsProfile := &awsecs.AWSProfile{}
		recursiveSetValue(awsProfile, resp.Attributes)

		awsProfiles = append(awsProfiles, awsProfile)
	}

	return awsProfiles, nil
}

func (db *SimpleDB) LoadAWSProfile(userId string) (*awsecs.AWSProfile, error) {
	domainName := getTypeString(db.Config, AWSPROFILE)
	simpledbSvc := simpledb.New(db.Sess)

	selectExpression := fmt.Sprintf("select * from `%s`", domainName)
	if userId != "" {
		selectExpression = selectExpression + fmt.Sprintf(" where UserId='%s'", userId)
	}

	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	awsProfiles := []*awsecs.AWSProfile{}
	selectOutput, err := simpledbSvc.Select(selectInput)
	if err != nil {
		return nil, errors.New("Unable to select data from simpleDB: " + err.Error())
	}

	for _, item := range selectOutput.Items {
		getAttributesInput := &simpledb.GetAttributesInput{
			DomainName: aws.String(domainName),
			ItemName:   item.Name,
		}

		resp, err := simpledbSvc.GetAttributes(getAttributesInput)
		if err != nil {
			return nil, errors.New("Unable to get attributes from simpleDB: " + err.Error())
		}

		awsProfile := &awsecs.AWSProfile{}
		recursiveSetValue(awsProfile, resp.Attributes)

		awsProfiles = append(awsProfiles, awsProfile)
	}

	return awsProfiles[0], nil
}

func (db *SimpleDB) DeleteAWSProfile(userId string) error {
	domainName := getTypeString(db.Config, AWSPROFILE)
	simpledbSvc := simpledb.New(db.Sess)

	selectExpression := fmt.Sprintf("select * from `%s` where UserId='%s'", domainName, userId)
	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	selectOutput, err := simpledbSvc.Select(selectInput)
	if err != nil {
		return errors.New("Unable to select data from simpleDB: " + err.Error())
	}

	if len(selectOutput.Items) == 0 {
		return fmt.Errorf("Unable to find %s data from simpleDB...", userId)
	}

	deleteAttributesInput := &simpledb.DeleteAttributesInput{
		DomainName: aws.String(domainName),
		ItemName:   selectOutput.Items[0].Name,
	}

	if _, err := simpledbSvc.DeleteAttributes(deleteAttributesInput); err != nil {
		return fmt.Errorf("Unable to delete %s attributes from simpleDB: %s", userId, err.Error())
	}

	return nil
}

func recursiveSetValue(v interface{}, attributes []*simpledb.Attribute) {
	modelReflect := reflect.ValueOf(v).Elem()
	modelRefType := modelReflect.Type()
	fieldsCount := modelReflect.NumField()

	for i := 0; i < fieldsCount; i++ {
		field := modelReflect.Field(i)
		fieldName := modelRefType.Field(i).Name

		switch field.Kind() {
		case reflect.Ptr:
			if fieldName == "K8SDeployment" {
				recursiveSetValue(field.Interface().(*awsecs.K8SStoreDeployment), attributes)
			}
		default:
			attrValue := restoreValue(fieldName, attributes)
			field.Set(reflect.ValueOf(attrValue))
		}
	}
}

func recursiveStructField(attrs *[]*simpledb.ReplaceableAttribute, v interface{}) {
	if v == nil || reflect.ValueOf(v).IsNil() {
		return
	}

	modelReflect := reflect.ValueOf(v).Elem()
	modelRefType := modelReflect.Type()
	fieldsCount := modelReflect.NumField()

	for i := 0; i < fieldsCount; i++ {
		field := modelReflect.Field(i)
		fieldName := modelRefType.Field(i).Name
		fieldValue := fmt.Sprintf("%v", field.Interface())

		switch field.Kind() {
		case reflect.Ptr:
			if fieldName == "K8SDeployment" {
				recursiveStructField(attrs, field.Interface().(*awsecs.K8SStoreDeployment))
			}
		default:
			appendAttributes(attrs, fieldName, fieldValue)
		}
	}
}

func restoreValue(fieldName string, attributes []*simpledb.Attribute) string {
	for _, attribute := range attributes {
		if fieldName == aws.StringValue(attribute.Name) {
			return aws.StringValue(attribute.Value)
		}
	}

	fieldValue := ""
	fieldInfos := map[string]string{}
	for _, attribute := range attributes {
		attrName := aws.StringValue(attribute.Name)
		attrValue := aws.StringValue(attribute.Value)

		if strings.Contains(attrName, fieldName+"_") {
			attrNames := strings.Split(attrName, "_")
			fieldInfos[attrNames[1]] = attrValue
		}
	}

	cnt := len(fieldInfos)
	for i := 1; i <= cnt; i++ {
		fieldValue = fieldValue + fieldInfos[strconv.Itoa(i)]
	}

	return fieldValue
}

func appendAttributes(attrs *[]*simpledb.ReplaceableAttribute, fieldName string, fieldValue string) {
	// SimpleDB attribute value Can not be greater than 1024
	splitLen := 1024
	if len(fieldValue) > splitLen {
		valLen := 0
		if len(fieldValue)%splitLen == 0 {
			valLen = len(fieldValue) / splitLen
		} else {
			valLen = len(fieldValue)/splitLen + 1
		}

		for i := 0; i < valLen; i++ {
			lastLndex := (i + 1) * splitLen
			if lastLndex > len(fieldValue) {
				lastLndex = len(fieldValue)
			}

			*attrs = append(*attrs, &simpledb.ReplaceableAttribute{
				Name:    aws.String(fmt.Sprintf("%s_%s", fieldName, strconv.Itoa(i+1))),
				Value:   aws.String(fieldValue[i*splitLen : lastLndex]),
				Replace: aws.Bool(true),
			})
		}
		return
	}

	*attrs = append(*attrs, &simpledb.ReplaceableAttribute{
		Name:    aws.String(fieldName),
		Value:   aws.String(fieldValue),
		Replace: aws.Bool(true),
	})
}

func createSessionByRegion(viper *viper.Viper, regionName string) (*session.Session, error) {
	awsId := viper.GetString("awsId")
	awsSecret := viper.GetString("awsSecret")
	creds := credentials.NewStaticCredentials(awsId, awsSecret, "")
	config := &aws.Config{
		Region: aws.String(regionName),
	}
	config = config.WithCredentials(creds)
	sess, err := session.NewSession(config)
	if err != nil {
		glog.Errorf("Unable to create session: %s", err)
		return nil, err
	}

	return sess, nil
}

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
	"github.com/hyperpilotio/deployer/kubernetes"
	"github.com/spf13/viper"
)

type SimpleDB struct {
	Region string
	Domain string
	Sess   *session.Session
}

func NewSimpleDB(config *viper.Viper) (*SimpleDB, error) {
	region := strings.ToLower(config.GetString("store.region"))
	domain := config.GetString("store.domain")

	session, err := createSessionByRegion(config, region)
	if err != nil {
		return nil, err
	}

	return &SimpleDB{
		Region: region,
		Domain: domain,
		Sess:   session,
	}, nil
}

func (db *SimpleDB) StoreNewDeployment(deployment *StoreDeployment) error {
	simpledbSvc := simpledb.New(db.Sess)

	createDomainInput := &simpledb.CreateDomainInput{
		DomainName: aws.String(db.Domain),
	}

	if _, err := simpledbSvc.CreateDomain(createDomainInput); err != nil {
		return errors.New("Unable to create simpleDB domain: " + err.Error())
	}

	attributes := []*simpledb.ReplaceableAttribute{}
	recursiveStructField(&attributes, deployment)

	putAttributesInput := &simpledb.PutAttributesInput{
		Attributes: attributes,
		DomainName: aws.String(db.Domain),
		ItemName:   aws.String(deployment.Name),
	}

	if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
		return errors.New("Unable to put attributes to simpleDB: " + err.Error())
	}

	return nil
}

func (db *SimpleDB) LoadDeployment() ([]*StoreDeployment, error) {
	simpledbSvc := simpledb.New(db.Sess)

	selectExpression := fmt.Sprintf("select * from `%s`", db.Domain)
	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	deployments := []*StoreDeployment{}
	selectOutput, err := simpledbSvc.Select(selectInput)
	if err != nil {
		return nil, errors.New("Unable to select data from simpleDB: " + err.Error())
	}

	for _, item := range selectOutput.Items {
		getAttributesInput := &simpledb.GetAttributesInput{
			DomainName: aws.String(db.Domain),
			ItemName:   item.Name,
		}

		resp, err := simpledbSvc.GetAttributes(getAttributesInput)
		if err != nil {
			return nil, errors.New("Unable to get attributes from simpleDB: " + err.Error())
		}

		deployment := &StoreDeployment{
			ECSDeployment: &awsecs.StoreDeployment{},
			K8SDeployment: &kubernetes.StoreDeployment{},
		}
		recursiveSetValue(deployment, resp.Attributes)

		deployments = append(deployments, deployment)
	}

	return deployments, nil
}

func (db *SimpleDB) DeleteDeployment(deploymentName string) error {
	simpledbSvc := simpledb.New(db.Sess)

	selectExpression := fmt.Sprintf("select * from `%s` where itemName()='%s'", db.Domain, deploymentName)
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
		DomainName: aws.String(db.Domain),
		ItemName:   selectOutput.Items[0].Name,
	}

	if _, err := simpledbSvc.DeleteAttributes(deleteAttributesInput); err != nil {
		return fmt.Errorf("Unable to delete %s attributes from simpleDB: %s", deploymentName, err.Error())
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
			if fieldName == "ECSDeployment" {
				recursiveSetValue(field.Interface().(*awsecs.StoreDeployment), attributes)
			}
			if fieldName == "K8SDeployment" {
				recursiveSetValue(field.Interface().(*kubernetes.StoreDeployment), attributes)
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
			if fieldName == "ECSDeployment" {
				recursiveStructField(attrs, field.Interface().(*awsecs.StoreDeployment))
			}
			if fieldName == "K8SDeployment" {
				recursiveStructField(attrs, field.Interface().(*kubernetes.StoreDeployment))
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

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
	"github.com/spf13/viper"
)

func getDomainName(name string, config *viper.Viper) string {
	return name + config.GetString("store.domainPostfix")
}

type SimpleDB struct {
	Name        string
	domainName  string
	Region      string
	Config      *viper.Viper
	simpledbSvc *simpledb.SimpleDB
}

func NewSimpleDB(name string, config *viper.Viper) (*SimpleDB, error) {
	region := strings.ToLower(config.GetString("store.region"))
	session, err := createSessionByRegion(config, region)
	if err != nil {
		return nil, errors.New("Unable to create aws session: " + err.Error())
	}

	simpledbSvc := simpledb.New(session)
	domainName := getDomainName(name, config)
	if err := createDomain(simpledbSvc, config, domainName); err != nil {
		return nil, errors.New("Unable to create simpledb domain: " + err.Error())
	}

	return &SimpleDB{
		Name:        name,
		Region:      region,
		Config:      config,
		simpledbSvc: simpledbSvc,
		domainName:  domainName,
	}, nil
}

func createDomain(simpledbSvc *simpledb.SimpleDB, config *viper.Viper, domainName string) error {
	createDomainInput := &simpledb.CreateDomainInput{
		DomainName: aws.String(domainName),
	}

	if _, err := simpledbSvc.CreateDomain(createDomainInput); err != nil {
		return errors.New("Unable to create simpleDB domain: " + err.Error())
	}

	return nil
}

func (db *SimpleDB) Store(key string, object interface{}) error {
	attributes := []*simpledb.ReplaceableAttribute{}
	recursiveStructField(&attributes, object)

	putAttributesInput := &simpledb.PutAttributesInput{
		Attributes: attributes,
		DomainName: aws.String(db.domainName),
		ItemName:   aws.String(key),
	}

	if _, err := db.simpledbSvc.PutAttributes(putAttributesInput); err != nil {
		return errors.New("Unable to put attributes to simpleDB: " + err.Error())
	}

	return nil
}

func (db *SimpleDB) LoadAll(f func() interface{}) (interface{}, error) {
	selectExpression := fmt.Sprintf("select * from `%s`", db.domainName)
	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	selectOutput, err := db.simpledbSvc.Select(selectInput)
	if err != nil {
		return nil, errors.New("Unable to select data from simpleDB: " + err.Error())
	}

	items := []interface{}{}
	for _, item := range selectOutput.Items {
		getAttributesInput := &simpledb.GetAttributesInput{
			DomainName: aws.String(db.domainName),
			ItemName:   item.Name,
		}

		resp, err := db.simpledbSvc.GetAttributes(getAttributesInput)
		if err != nil {
			return nil, errors.New("Unable to get attributes from simpleDB: " + err.Error())
		}

		v := f()
		recursiveSetValue(v, resp.Attributes)

		items = append(items, v)
	}

	return items, nil
}

func (db *SimpleDB) Delete(key string) error {
	selectExpression := fmt.Sprintf("select * from `%s` where itemName()='%s'", db.domainName, key)
	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	selectOutput, err := db.simpledbSvc.Select(selectInput)
	if err != nil {
		return errors.New("Unable to select data from simpleDB: " + err.Error())
	}

	if len(selectOutput.Items) == 0 {
		return fmt.Errorf("Unable to find %s data from simpleDB", key)
	}

	deleteAttributesInput := &simpledb.DeleteAttributesInput{
		DomainName: aws.String(db.domainName),
		ItemName:   selectOutput.Items[0].Name,
	}

	if _, err := db.simpledbSvc.DeleteAttributes(deleteAttributesInput); err != nil {
		return fmt.Errorf("Unable to delete %s attributes from simpleDB: %s", key, err.Error())
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
		case reflect.Interface:
			recursiveSetValue(field.Interface(), attributes)
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
		case reflect.Interface:
			recursiveStructField(attrs, field.Interface())
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

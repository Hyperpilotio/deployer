package kubernetes

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/common"

	"github.com/spf13/viper"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"

	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type KubernetesDeployment struct {
	BastionIp       string
	MasterIp        string
	KubeConfigPath  string
	Endpoints       map[string]string
	KubeConfig      *rest.Config
	DeployedCluster *awsecs.DeployedCluster
}

type CreateDeploymentResponse struct {
	Name      string            `json:"name"`
	Endpoints map[string]string `json:"endpoints"`
	BastionIp string            `json:"bastionIp"`
	MasterIp  string            `json:"masterIp"`
}

type DeploymentLoadBalancers struct {
	StackName             string
	ApiServerBalancerName string
	LoadBalancerNames     []string
}

type KubernetesClusters struct {
	Clusters map[string]*KubernetesDeployment
}

var publicPortType = 1

func NewKubernetesClusters() *KubernetesClusters {
	return &KubernetesClusters{
		Clusters: make(map[string]*KubernetesDeployment),
	}
}

func (k8sDeployment *KubernetesDeployment) populateNodeInfos(ec2Svc *ec2.EC2) error {
	describeInstancesInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String("k8s-node"),
				},
			},
			{
				Name: aws.String("tag:KubernetesCluster"),
				Values: []*string{
					aws.String(k8sDeployment.DeployedCluster.StackName()),
				},
			},
			{
				Name: aws.String("instance-state-name"),
				Values: []*string{
					aws.String("running"),
				},
			},
		},
	}
	describeInstancesOutput, describeErr := ec2Svc.DescribeInstances(describeInstancesInput)
	if describeErr != nil {
		return errors.New("Unable to describe ec2 instances: " + describeErr.Error())
	}

	i := 1
	for _, reservation := range describeInstancesOutput.Reservations {
		for _, instance := range reservation.Instances {
			nodeInfo := &awsecs.NodeInfo{
				Instance:  instance,
				PrivateIp: *instance.PrivateIpAddress,
			}
			k8sDeployment.DeployedCluster.NodeInfos[i] = nodeInfo
			i += 1
		}
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) uploadFiles(ec2Svc *ec2.EC2, uploadedFiles map[string]string) error {
	log := k8sDeployment.DeployedCluster.Logger
	if len(k8sDeployment.DeployedCluster.Deployment.Files) == 0 {
		return nil
	}

	deployedCluster := k8sDeployment.DeployedCluster

	clientConfig, clientConfigErr := deployedCluster.SshConfig("ubuntu")
	if clientConfigErr != nil {
		return errors.New("Unable to create ssh config: " + clientConfigErr.Error())
	}

	for _, nodeInfo := range deployedCluster.NodeInfos {
		sshClient := common.NewSshClient(nodeInfo.PrivateIp+":22", clientConfig, k8sDeployment.BastionIp+":22")
		// TODO: Refactor this so can be reused with AWS
		for _, deployFile := range deployedCluster.Deployment.Files {
			// TODO: Bulk upload all files, where ssh client needs to support multiple files transfer
			// in the same connection
			location, ok := uploadedFiles[deployFile.FileId]
			if !ok {
				return errors.New("Unable to find uploaded file " + deployFile.FileId)
			}

			if err := sshClient.CopyLocalFileToRemote(location, deployFile.Path); err != nil {
				return fmt.Errorf("Unable to upload file %s to server %s: %s",
					deployFile.FileId, nodeInfo.PrivateIp, err.Error())
			}
		}
	}

	log.Info("Uploaded all files")
	return nil
}

func (k8sDeployment *KubernetesDeployment) getExistingNamespaces(k8sClient *k8s.Clientset) (map[string]bool, error) {
	namespaces := map[string]bool{}
	k8sNamespaces := k8sClient.CoreV1().Namespaces()
	existingNamespaces, err := k8sNamespaces.List(v1.ListOptions{})
	if err != nil {
		return namespaces, fmt.Errorf("Unable to get existing namespaces: " + err.Error())
	}

	for _, existingNamespace := range existingNamespaces.Items {
		namespaces[existingNamespace.Name] = true
	}

	return namespaces, nil
}

func (k8sDeployment *KubernetesDeployment) deployCluster(config *viper.Viper, uploadedFiles map[string]string) error {
	deployedCluster := k8sDeployment.DeployedCluster
	sess, sessionErr := awsecs.CreateSession(config, deployedCluster.Deployment)
	if sessionErr != nil {
		return errors.New("Unable to create session: " + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)

	if err := awsecs.CreateKeypair(ec2Svc, deployedCluster); err != nil {
		return errors.New("Unable to create key pair: " + err.Error())
	}

	if err := k8sDeployment.deployKubernetes(sess); err != nil {
		return errors.New("Unable to deploy kubernetes custer: " + err.Error())
	}

	if err := k8sDeployment.populateNodeInfos(ec2Svc); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	if err := k8sDeployment.uploadFiles(ec2Svc, uploadedFiles); err != nil {
		k8sDeployment.deleteDeploymentOnFailure(config)
		return errors.New("Unable to upload files to cluster: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(k8sDeployment.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	if err := k8sDeployment.tagKubeNodes(k8sClient); err != nil {
		k8sDeployment.deleteDeploymentOnFailure(config)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := k8sDeployment.deployKubernetesObjects(config, k8sClient, false); err != nil {
		k8sDeployment.deleteDeploymentOnFailure(config)
		return errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) deployKubernetesObjects(config *viper.Viper, k8sClient *k8s.Clientset, skipDelete bool) error {
	namespaces, namespacesErr := k8sDeployment.getExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		if !skipDelete {
			k8sDeployment.deleteDeploymentOnFailure(config)
		}
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	if err := k8sDeployment.createSecrets(k8sClient, namespaces); err != nil {
		if !skipDelete {
			k8sDeployment.deleteDeploymentOnFailure(config)
		}
		return errors.New("Unable to create secrets in k8s: " + err.Error())
	}

	if err := k8sDeployment.deployServices(k8sClient, namespaces); err != nil {
		if !skipDelete {
			k8sDeployment.deleteDeploymentOnFailure(config)
		}
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	k8sDeployment.recordPublicEndpoints(k8sClient)

	return nil
}

func (k8sDeployment *KubernetesDeployment) deployKubernetes(sess *session.Session) error {
	log := k8sDeployment.DeployedCluster.Logger
	cfSvc := cloudformation.New(sess)
	params := &cloudformation.CreateStackInput{
		StackName: aws.String(k8sDeployment.DeployedCluster.StackName()),
		Capabilities: []*string{
			aws.String("CAPABILITY_NAMED_IAM"),
		},
		Parameters: []*cloudformation.Parameter{
			{
				ParameterKey:   aws.String("AdminIngressLocation"),
				ParameterValue: aws.String("0.0.0.0/0"),
			},
			{
				ParameterKey:   aws.String("KeyName"),
				ParameterValue: aws.String(k8sDeployment.DeployedCluster.KeyName()),
			},
			{
				ParameterKey:   aws.String("NetworkingProvider"),
				ParameterValue: aws.String("weave"),
			},
			{
				ParameterKey: aws.String("K8sNodeCapacity"),
				ParameterValue: aws.String(
					strconv.Itoa(
						len(k8sDeployment.DeployedCluster.Deployment.ClusterDefinition.Nodes))),
			},
			{
				ParameterKey: aws.String("InstanceType"),
				ParameterValue: aws.String(
					k8sDeployment.DeployedCluster.Deployment.ClusterDefinition.Nodes[0].InstanceType),
			},
			{
				ParameterKey:   aws.String("DiskSizeGb"),
				ParameterValue: aws.String("40"),
			},
			{
				ParameterKey:   aws.String("BastionInstanceType"),
				ParameterValue: aws.String("t2.micro"),
			},
			{
				ParameterKey:   aws.String("QSS3BucketName"),
				ParameterValue: aws.String("heptio-aws-quickstart-test"),
			},
			{
				ParameterKey:   aws.String("QSS3KeyPrefix"),
				ParameterValue: aws.String("heptio/kubernetes/latest"),
			},
			{
				ParameterKey:   aws.String("AvailabilityZone"),
				ParameterValue: aws.String(k8sDeployment.DeployedCluster.Deployment.Region + "b"),
			},
		},
		Tags: []*cloudformation.Tag{
			{
				Key:   aws.String("deployment"),
				Value: aws.String(k8sDeployment.DeployedCluster.Deployment.Name),
			},
		},
		TemplateURL:      aws.String("https://hyperpilot-snap-collectors.s3.amazonaws.com/kubernetes-cluster-with-new-vpc-1.5.5.template"),
		TimeoutInMinutes: aws.Int64(30),
	}
	log.Info("Creating kubernetes stack...")
	if _, err := cfSvc.CreateStack(params); err != nil {
		return errors.New("Unable to create stack: " + err.Error())
	}

	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(k8sDeployment.DeployedCluster.StackName()),
	}

	log.Info("Waiting until stack is completed...")
	if err := cfSvc.WaitUntilStackCreateComplete(describeStacksInput); err != nil {
		return errors.New("Unable to wait until stack complete: " + err.Error())
	}

	log.Info("Kuberenete stack completed")
	describeStacksOutput, describeStacksErr := cfSvc.DescribeStacks(describeStacksInput)
	if describeStacksErr != nil {
		return errors.New("Unable to get stack outputs: " + describeStacksErr.Error())
	}

	outputs := describeStacksOutput.Stacks[0].Outputs
	sshProxyCommand := ""
	getKubeConfigCommand := ""
	for _, output := range outputs {
		if *output.OutputKey == "SSHProxyCommand" {
			sshProxyCommand = *output.OutputValue
		} else if *output.OutputKey == "GetKubeConfigCommand" {
			getKubeConfigCommand = *output.OutputValue
		}
	}

	if sshProxyCommand == "" {
		return errors.New("Unable to find SSHProxyCommand in stack output")
	} else if getKubeConfigCommand == "" {
		return errors.New("Unable to find GetKubeConfigCommand in stack output")
	}

	// We expect the SSHProxyCommand to be in the following format:
	//"ssh -A -L8080:localhost:8080 -o ProxyCommand='ssh ubuntu@111.111.111.111 nc 10.0.0.0 22' ubuntu@10.0.0.0"
	addresses := []string{}
	for _, part := range strings.Split(sshProxyCommand, " ") {
		if strings.HasPrefix(part, "ubuntu@") {
			addresses = append(addresses, strings.Split(part, "@")[1])
		}
	}

	if len(addresses) != 2 {
		return errors.New("Unexpected ssh proxy command format: " + sshProxyCommand)
	}

	k8sDeployment.BastionIp = addresses[0]
	k8sDeployment.MasterIp = addresses[1]

	if err := k8sDeployment.downloadKubeConfig(); err != nil {
		return errors.New("Unable to download kubeconfig: " + err.Error())
	}

	log.Infof("Downloaded kube config at %s", k8sDeployment.KubeConfigPath)

	if kubeConfig, err := clientcmd.BuildConfigFromFlags("", k8sDeployment.KubeConfigPath); err != nil {
		return errors.New("Unable to parse kube config: " + err.Error())
	} else {
		k8sDeployment.KubeConfig = kubeConfig
	}

	return nil
}

// CreateDeployment start a deployment
func (k8sClusters *KubernetesClusters) CreateDeployment(
	config *viper.Viper,
	uploadedFiles map[string]string,
	deployedCluster *awsecs.DeployedCluster) (*CreateDeploymentResponse, error) {
	log := deployedCluster.Logger
	log.Info("Starting kubernetes deployment")

	k8sDeployment := &KubernetesDeployment{
		DeployedCluster: deployedCluster,
	}

	k8sClusters.Clusters[deployedCluster.Deployment.Name] = k8sDeployment
	if err := k8sDeployment.deployCluster(config, uploadedFiles); err != nil {
		return nil, errors.New("Unable to deploy kubernetes: " + err.Error())
	}

	response := &CreateDeploymentResponse{
		Name:      deployedCluster.Deployment.Name,
		Endpoints: k8sDeployment.Endpoints,
		BastionIp: k8sDeployment.BastionIp,
		MasterIp:  k8sDeployment.MasterIp,
	}

	return response, nil
}

// UpdateDeployment start a deployment on EC2 is ready
func (k8sClusters *KubernetesClusters) UpdateDeployment(config *viper.Viper, deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) error {
	log := deployedCluster.Logger
	k8sDeployment, ok := k8sClusters.Clusters[deployedCluster.Deployment.Name]
	if !ok {
		return fmt.Errorf("Unable to find cluster '%s' to update", deployedCluster.Deployment.Name)
	}

	log.Info("Updating kubernetes deployment")

	k8sClient, err := k8s.NewForConfig(k8sDeployment.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	if err := k8sDeployment.deleteK8S(k8sDeployment.getAllDeployedNamespaces(), k8sDeployment.KubeConfig); err != nil {
		log.Warningf("Unable to delete k8s objects in update: " + err.Error())
	}

	sess, sessionErr := awsecs.CreateSession(config, deployedCluster.Deployment)
	if sessionErr != nil {
		return errors.New("Unable to create aws session for delete: " + sessionErr.Error())
	}

	elbSvc := elb.New(sess)
	ec2Svc := ec2.New(sess)

	if err := k8sDeployment.deleteLoadBalancers(elbSvc, true); err != nil {
		log.Warningf("Unable to delete load balancers: " + err.Error())
	}

	if err := k8sDeployment.deleteElbSecurityGroup(ec2Svc); err != nil {
		return fmt.Errorf("Unable to deleting elb securityGroups: %s", err.Error())
	}

	if err := k8sDeployment.deployKubernetesObjects(config, k8sClient, true); err != nil {
		log.Warningf("Unable to deploy k8s objects in update: " + err.Error())
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) deleteSecurityGroup(ec2Svc *ec2.EC2, vpcID *string) error {
	log := k8sDeployment.DeployedCluster.Logger
	errBool := false
	describeParams := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{vpcID},
			},
		},
	}

	resp, err := ec2Svc.DescribeSecurityGroups(describeParams)
	if err != nil {
		return fmt.Errorf("Unable to describe tags of security group: %s\n", err.Error())
	}

	for _, group := range resp.SecurityGroups {
		if aws.StringValue(group.GroupName) == "default" {
			continue
		}
		params := &ec2.DeleteSecurityGroupInput{
			GroupId: group.GroupId,
		}
		if _, err := ec2Svc.DeleteSecurityGroup(params); err != nil {
			log.Warningf("Unable to delete security group: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to delete all the relative security groups")
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) waitUntilInternetGatewayDeleted(ec2Svc *ec2.EC2, deploymentName string, timeout time.Duration) error {
	log := k8sDeployment.DeployedCluster.Logger
	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				params := &ec2.DescribeTagsInput{
					Filters: []*ec2.Filter{
						{
							Name: aws.String("resource-type"),
							Values: []*string{
								aws.String("internet-gateway"),
							},
						},
						{
							Name: aws.String("tag:deployment"),
							Values: []*string{
								aws.String(deploymentName),
							},
						},
					},
				}

				resp, _ := ec2Svc.DescribeTags(params)
				if resp.Tags == nil {
					c <- true
				}
				time.Sleep(time.Second * 10)
			}
		}
	}()

	select {
	case <-c:
		log.Info("InternetGateway is deleted")
		return nil
	case <-time.After(timeout):
		quit <- true
		return errors.New("Timed out waiting for InternetGateway to be deleted")
	}
}

func (k8sDeployment *KubernetesDeployment) waitUntilNetworkInterfaceAvailabled(ec2Svc *ec2.EC2, securityGroupNames []*string, timeout time.Duration) error {
	log := k8sDeployment.DeployedCluster.Logger
	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				params := &ec2.DescribeNetworkInterfacesInput{
					Filters: []*ec2.Filter{
						{
							Name:   aws.String("group-name"),
							Values: securityGroupNames,
						},
					},
				}

				allAvailabled := true
				resp, _ := ec2Svc.DescribeNetworkInterfaces(params)
				for _, nif := range resp.NetworkInterfaces {
					if aws.StringValue(nif.Status) != "available" {
						allAvailabled = false
					}
				}
				if allAvailabled || len(resp.NetworkInterfaces) == 0 {
					c <- true
				}
				time.Sleep(time.Second * 5)
			}
		}
	}()

	select {
	case <-c:
		log.Info("NetworkInterfaces is availabled")
		return nil
	case <-time.After(timeout):
		quit <- true
		return errors.New("Timed out waiting for NetworkInterfaces to be availabled")
	}
}

func (k8sDeployment *KubernetesDeployment) deleteNetworkInterfaces(ec2Svc *ec2.EC2, securityGroupNames []*string) error {
	log := k8sDeployment.DeployedCluster.Logger
	errBool := false

	describeNetworkInterfacesInput := &ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("group-name"),
				Values: securityGroupNames,
			},
		},
	}

	resp, err := ec2Svc.DescribeNetworkInterfaces(describeNetworkInterfacesInput)
	if err != nil {
		return fmt.Errorf("Unable to describe tags of network interface: %s\n", err.Error())
	}

	for _, nif := range resp.NetworkInterfaces {
		if nif.Attachment.AttachmentId != nil {
			detachNetworkInterfaceInput := &ec2.DetachNetworkInterfaceInput{
				AttachmentId: nif.Attachment.AttachmentId,
				Force:        aws.Bool(true),
			}
			if _, err := ec2Svc.DetachNetworkInterface(detachNetworkInterfaceInput); err != nil {
				return fmt.Errorf("Unable to detach network interface: %s\n", err.Error())
			}
		}
	}

	if err := k8sDeployment.waitUntilNetworkInterfaceAvailabled(ec2Svc, securityGroupNames, time.Duration(30)*time.Second); err != nil {
		return fmt.Errorf("Unable to wait until network interface is available: %s\n", err.Error())
	}

	for _, nif := range resp.NetworkInterfaces {
		deleteNetworkInterfaceInput := &ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: nif.NetworkInterfaceId,
		}
		if _, err := ec2Svc.DeleteNetworkInterface(deleteNetworkInterfaceInput); err != nil {
			log.Warningf("Unable to delete network interface: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to delete all the relative network interface")
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) deleteElbSecurityGroup(ec2Svc *ec2.EC2) error {
	log := k8sDeployment.DeployedCluster.Logger
	errBool := false

	stackName := k8sDeployment.DeployedCluster.StackName()
	describeParams := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:KubernetesCluster"),
				Values: []*string{aws.String(stackName)},
			},
		},
	}

	resp, err := ec2Svc.DescribeSecurityGroups(describeParams)
	if err != nil {
		return fmt.Errorf("Unable to describe tags of security group: %s\n", err.Error())
	}

	k8sClusterSgId := ""
	k8sElbSgIds := []*string{}
	k8sElbSgNames := []*string{}
	for _, securityGroup := range resp.SecurityGroups {
		if strings.HasPrefix(aws.StringValue(securityGroup.GroupName), "k8s-elb") {
			k8sElbSgNames = append(k8sElbSgNames, securityGroup.GroupName)
			k8sElbSgIds = append(k8sElbSgIds, securityGroup.GroupId)
		}

		for _, tag := range securityGroup.Tags {
			tagKey := aws.StringValue(tag.Key)
			tagVal := aws.StringValue(tag.Value)
			if (tagKey == "Name") && (tagVal == "k8s-cluster-security-group") {
				k8sClusterSgId = aws.StringValue(securityGroup.GroupId)
				break
			}
		}
	}

	if err := k8sDeployment.deleteNetworkInterfaces(ec2Svc, k8sElbSgNames); err != nil {
		return fmt.Errorf("Unable to delete network interfaces: %s\n", err.Error())
	}

	for _, sgId := range k8sElbSgIds {
		revokeSecurityGroupIngressInput := &ec2.RevokeSecurityGroupIngressInput{
			GroupId: aws.String(k8sClusterSgId),
			IpPermissions: []*ec2.IpPermission{
				{
					IpProtocol: aws.String("-1"),
					UserIdGroupPairs: []*ec2.UserIdGroupPair{
						{
							GroupId: sgId,
						},
					},
				},
			},
		}

		if _, err := ec2Svc.RevokeSecurityGroupIngress(revokeSecurityGroupIngressInput); err != nil {
			return fmt.Errorf("Unable to remove ingress rules from security group: %s", err.Error())
		}

		params := &ec2.DeleteSecurityGroupInput{
			GroupId: sgId,
		}
		if _, err := ec2Svc.DeleteSecurityGroup(params); err != nil {
			log.Warningf("Unable to delete security group: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to delete all the relative security groups")
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) deleteLoadBalancers(elbSvc *elb.ELB, skipApiServer bool) error {
	log := k8sDeployment.DeployedCluster.Logger
	log.Infof("Deleting loadBalancer...")

	deploymentLoadBalancers := &DeploymentLoadBalancers{
		StackName:         k8sDeployment.DeployedCluster.StackName(),
		LoadBalancerNames: []string{},
	}

	resp, _ := elbSvc.DescribeLoadBalancers(nil)
	for _, lbd := range resp.LoadBalancerDescriptions {
		loadBalancerName := aws.StringValue(lbd.LoadBalancerName)
		describeTagsInput := &elb.DescribeTagsInput{
			LoadBalancerNames: []*string{
				lbd.LoadBalancerName,
			},
		}

		tagsOutput, err := elbSvc.DescribeTags(describeTagsInput)
		if err != nil {
			return fmt.Errorf("Unable to describe loadBalancer tags: %s", err.Error())
		}

		tagInfos := map[string]string{}
		for _, tagDescription := range tagsOutput.TagDescriptions {
			for _, tag := range tagDescription.Tags {
				tagInfos[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
			}
		}

		if tagInfos["KubernetesCluster"] == k8sDeployment.DeployedCluster.StackName() {
			if tagInfos["kubernetes.io/service-name"] == "kube-system/apiserver-public" {
				deploymentLoadBalancers.ApiServerBalancerName = loadBalancerName
			}
			deploymentLoadBalancers.LoadBalancerNames = append(deploymentLoadBalancers.LoadBalancerNames, loadBalancerName)
		}
	}

	for _, loadBalancerName := range deploymentLoadBalancers.LoadBalancerNames {
		// Need to skip delete k8s-master elb loadbancerName when update deployment
		if skipApiServer {
			if loadBalancerName == deploymentLoadBalancers.ApiServerBalancerName {
				continue
			}
		}

		deleteLoadBalancerInput := &elb.DeleteLoadBalancerInput{
			LoadBalancerName: aws.String(loadBalancerName),
		}
		if _, err := elbSvc.DeleteLoadBalancer(deleteLoadBalancerInput); err != nil {
			return fmt.Errorf("Unable to deleting loadBalancer: %s", err.Error())
		}
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) deleteCloudFormationStack(elbSvc *elb.ELB, ec2Svc *ec2.EC2, cfSvc *cloudformation.CloudFormation, stackName string) error {
	log := k8sDeployment.DeployedCluster.Logger
	// find k8s-node stack name
	deploymentName := k8sDeployment.DeployedCluster.Deployment.Name
	describeStacksOutput, _ := cfSvc.DescribeStacks(nil)
	k8sNodeStackName := ""
	vpcID := ""
	for _, stack := range describeStacksOutput.Stacks {
		if strings.HasPrefix(aws.StringValue(stack.StackName), stackName+"-K8sStack") {
			k8sNodeStackName = aws.StringValue(stack.StackName)
			if vpcID == "" {
				for _, param := range stack.Parameters {
					if aws.StringValue(param.ParameterKey) == "VPCID" {
						vpcID = aws.StringValue(param.ParameterValue)
						break
					}
				}
			}
		} else if aws.StringValue(stack.StackName) == stackName {
			if vpcID == "" {
				for _, output := range stack.Outputs {
					if aws.StringValue(output.OutputKey) == "VPCID" {
						vpcID = aws.StringValue(output.OutputValue)
						break
					}
				}
			}
		}
	}

	// delete k8s-master/k8s-node stack
	if k8sNodeStackName != "" {
		log.Infof("Deleting k8s-master/k8s-node stack...")
		deleteStackInput := &cloudformation.DeleteStackInput{
			StackName: aws.String(k8sNodeStackName),
		}

		if _, err := cfSvc.DeleteStack(deleteStackInput); err != nil {
			log.Warningf("Unable to delete stack: %s", err.Error())
		}

		describeStacksInput := &cloudformation.DescribeStacksInput{
			StackName: aws.String(k8sNodeStackName),
		}

		if err := cfSvc.WaitUntilStackDeleteComplete(describeStacksInput); err != nil {
			log.Warningf("Unable to wait until stack is deleted: %s", err.Error())
		} else if err == nil {
			log.Infof("Delete %s stack ok...", k8sNodeStackName)
		}
	} else {
		log.Warningf("Unable to find k8s-master/k8s-node stack...")
	}

	if err := k8sDeployment.deleteLoadBalancers(elbSvc, false); err != nil {
		log.Warningf("Unable to delete load balancers: " + err.Error())
	}

	// delete bastion-Host EC2 instance
	log.Infof("Deleting bastion-Host EC2 instance...")
	if err := deleteBastionHost(ec2Svc, deploymentName); err != nil {
		log.Warningf("Unable to delete bastion-Host EC2 instance: %s", err.Error())
	}

	// delete bastion-host stack
	log.Infof("Deleting bastion-host stack...")
	deleteBastionHostStackInput := &cloudformation.DeleteStackInput{
		StackName: aws.String(stackName),
	}

	if _, err := cfSvc.DeleteStack(deleteBastionHostStackInput); err != nil {
		log.Warningf("Unable to delete stack: %s", err.Error())
	}

	if err := k8sDeployment.waitUntilInternetGatewayDeleted(ec2Svc, deploymentName, time.Duration(3)*time.Minute); err != nil {
		log.Warningf("Unable to wait for internetGateway to be deleted: %s", err.Error())
	}

	// delete securityGroup
	retryTimes := 5
	log.Infof("Deleteing securityGroup...")
	for i := 1; i <= retryTimes; i++ {
		if err := k8sDeployment.deleteSecurityGroup(ec2Svc, aws.String(vpcID)); err != nil {
			log.Warningf("Unable to delete securityGroup: %s, retrying %d time", err.Error(), i)
		} else if err == nil {
			log.Infof("Delete securityGroup ok...")
			break
		}
		time.Sleep(time.Duration(30) * time.Second)
	}

	describeBastionHostStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(stackName),
	}

	if err := cfSvc.WaitUntilStackDeleteComplete(describeBastionHostStacksInput); err != nil {
		log.Warningf("Unable to wait until stack is deleted: %s", err.Error())
	} else if err == nil {
		log.Infof("Delete %s stack ok...", stackName)
	}

	return nil
}

func deleteBastionHost(ec2Svc *ec2.EC2, deploymentName string) error {
	describeInstancesInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String("bastion-host"),
				},
			},
			{
				Name: aws.String("tag:deployment"),
				Values: []*string{
					aws.String(deploymentName),
				},
			},
			{
				Name: aws.String("instance-state-name"),
				Values: []*string{
					aws.String("running"),
				},
			},
		},
	}

	describeInstancesOutput, describeErr := ec2Svc.DescribeInstances(describeInstancesInput)
	if describeErr != nil {
		return errors.New("Unable to describe ec2 instances: " + describeErr.Error())
	}

	var instanceIds []*string
	for _, reservation := range describeInstancesOutput.Reservations {
		for _, instance := range reservation.Instances {
			instanceIds = append(instanceIds, instance.InstanceId)
		}
	}

	params := &ec2.TerminateInstancesInput{
		InstanceIds: instanceIds,
	}

	if _, err := ec2Svc.TerminateInstances(params); err != nil {
		return fmt.Errorf("Unable to terminate EC2 instance: %s\n", err.Error())
	}

	terminatedInstanceParams := &ec2.DescribeInstancesInput{
		InstanceIds: instanceIds,
	}

	if err := ec2Svc.WaitUntilInstanceTerminated(terminatedInstanceParams); err != nil {
		return fmt.Errorf("Unable to wait until EC2 instance terminated: %s\n", err.Error())
	}
	return nil
}

func (k8sDeployment *KubernetesDeployment) deleteK8S(namespaces []string, kubeConfig *rest.Config) error {
	log := k8sDeployment.DeployedCluster.Logger
	if kubeConfig == nil {
		return errors.New("Empty kubeconfig passed, skipping to delete k8s objects")
	}

	log.Info("Found kube config, deleting kubernetes objects")
	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	for _, namespace := range namespaces {
		glog.Info("Deleting kubernetes objects in namespace " + namespace)
		daemonsets := k8sClient.Extensions().DaemonSets(namespace)
		if daemonsetList, listError := daemonsets.List(v1.ListOptions{}); listError == nil {
			for _, daemonset := range daemonsetList.Items {
				name := daemonset.GetObjectMeta().GetName()
				if err := daemonsets.Delete(name, &v1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete daemonset %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list daemonsets in namespace '%s' for deletion: ", namespace, listError.Error())
		}

		deploys := k8sClient.Extensions().Deployments(namespace)
		if deployLists, listError := deploys.List(v1.ListOptions{}); listError == nil {
			for _, deployment := range deployLists.Items {
				name := deployment.GetObjectMeta().GetName()
				if err := deploys.Delete(name, &v1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete deployment %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list deployments in namespace '%s' for deletion: ", namespace, listError.Error())
		}

		replicaSets := k8sClient.Extensions().ReplicaSets(namespace)
		if replicaSetList, listError := replicaSets.List(v1.ListOptions{}); listError == nil {
			for _, replicaSet := range replicaSetList.Items {
				name := replicaSet.GetObjectMeta().GetName()
				if err := replicaSets.Delete(name, &v1.DeleteOptions{}); err != nil {
					glog.Warningf("Unable to delete replica set %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list replica sets in namespace '%s' for deletion: ", namespace, listError.Error())
		}

		services := k8sClient.CoreV1().Services(namespace)
		if serviceLists, listError := services.List(v1.ListOptions{}); listError == nil {
			for _, service := range serviceLists.Items {
				serviceName := service.GetObjectMeta().GetName()
				if err := services.Delete(serviceName, &v1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete service %s: %s", serviceName, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list services in namespace '%s' for deletion: %s", namespace, listError.Error())
		}

		pods := k8sClient.CoreV1().Pods(namespace)
		if podLists, listError := pods.List(v1.ListOptions{}); listError == nil {
			for _, pod := range podLists.Items {
				podName := pod.GetObjectMeta().GetName()
				if err := pods.Delete(podName, &v1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete pod %s: %s", podName, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list pods in namespace '%s' for deletion: %s", namespace, listError.Error())
		}

		secrets := k8sClient.CoreV1().Secrets(namespace)
		if secretList, listError := secrets.List(v1.ListOptions{}); listError == nil {
			for _, secret := range secretList.Items {
				name := secret.GetObjectMeta().GetName()
				if err := secrets.Delete(name, &v1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete service %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list secrets in namespace '%s' for deletion: %s", namespace, listError.Error())
		}

	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) deleteDeploymentOnFailure(config *viper.Viper) {
	log := k8sDeployment.DeployedCluster.Logger
	if k8sDeployment.DeployedCluster.Deployment.KubernetesDeployment.SkipDeleteOnFailure {
		log.Warning("Skipping delete deployment on failure")
		return
	}

	k8sDeployment.deleteDeployment(config)
}

func (k8sDeployment *KubernetesDeployment) deleteDeployment(config *viper.Viper) {
	deployedCluster := k8sDeployment.DeployedCluster
	log := deployedCluster.Logger
	// Deleting kubernetes deployment
	log.Infof("Deleting kubernetes deployment...")
	if err := k8sDeployment.deleteK8S(k8sDeployment.getAllDeployedNamespaces(), k8sDeployment.KubeConfig); err != nil {
		log.Warningf("Unable to deleting kubernetes deployment: %s", err.Error())
	}

	sess, sessionErr := awsecs.CreateSession(config, deployedCluster.Deployment)
	if sessionErr != nil {
		log.Warningf("Unable to create aws session for delete: %s", sessionErr.Error())
		return
	}

	elbSvc := elb.New(sess)
	cfSvc := cloudformation.New(sess)
	ec2Svc := ec2.New(sess)

	// delete cloudformation Stack
	cfStackName := deployedCluster.StackName()
	log.Infof("Deleting cloudformation Stack: %s", cfStackName)
	if err := k8sDeployment.deleteCloudFormationStack(elbSvc, ec2Svc, cfSvc, cfStackName); err != nil {
		log.Warningf("Unable to deleting cloudformation Stack: %s", err.Error())
	}

	log.Infof("Deleting KeyPair...")
	if err := awsecs.DeleteKeyPair(ec2Svc, deployedCluster); err != nil {
		log.Warning("Unable to delete key pair: " + err.Error())
	}
}

// DeleteDeployment clean up the cluster from kubenetes.
func (k8sClusters *KubernetesClusters) DeleteDeployment(config *viper.Viper, deployedCluster *awsecs.DeployedCluster) {
	log := deployedCluster.Logger
	k8sDeployment, ok := k8sClusters.Clusters[deployedCluster.Deployment.Name]
	if !ok {
		log.Warningf("Unable to find kubernetes deployment to delete: %s", deployedCluster.Deployment.Name)
		return
	}

	k8sDeployment.deleteDeployment(config)

	delete(k8sClusters.Clusters, deployedCluster.Deployment.Name)
}

func (k8sDeployment *KubernetesDeployment) getAllDeployedNamespaces() []string {
	// Find all namespaces we deployed to
	allNamespaces := []string{}
	for _, task := range k8sDeployment.DeployedCluster.Deployment.KubernetesDeployment.Kubernetes {
		newNamespace := ""
		if task.Deployment != nil {
			newNamespace = getNamespace(task.Deployment.ObjectMeta)
		} else if task.DaemonSet != nil {
			newNamespace = getNamespace(task.DaemonSet.ObjectMeta)
		}
		exists := false
		for _, namespace := range allNamespaces {
			if namespace == newNamespace {
				exists = true
				break
			}
		}

		if !exists {
			allNamespaces = append(allNamespaces, newNamespace)
		}
	}

	return allNamespaces
}

func (k8sDeployment *KubernetesDeployment) tagKubeNodes(k8sClient *k8s.Clientset) error {
	log := k8sDeployment.DeployedCluster.Logger
	nodeInfos := map[string]int{}
	for _, mapping := range k8sDeployment.DeployedCluster.Deployment.NodeMapping {
		privateDnsName := k8sDeployment.DeployedCluster.NodeInfos[mapping.Id].Instance.PrivateDnsName
		nodeInfos[aws.StringValue(privateDnsName)] = mapping.Id
	}

	for nodeName, id := range nodeInfos {
		if node, err := k8sClient.CoreV1().Nodes().Get(nodeName); err == nil {
			node.Labels["hyperpilot/node-id"] = strconv.Itoa(id)
			node.Labels["hyperpilot/deployment"] = k8sDeployment.DeployedCluster.Name
			if _, err := k8sClient.CoreV1().Nodes().Update(node); err == nil {
				log.Infof("Added label hyperpilot/node-id:%s to Kubernetes node %s", strconv.Itoa(id), nodeName)
			}
		} else {
			return fmt.Errorf("Unable to get Kubernetes node by name %s: %s", nodeName, err.Error())
		}
	}

	return nil
}

func getNamespace(objectMeta v1.ObjectMeta) string {
	namespace := objectMeta.Namespace
	if namespace == "" {
		return "default"
	}

	return namespace
}

func createNamespaceIfNotExist(namespace string, existingNamespaces map[string]bool, k8sClient *k8s.Clientset) error {
	if _, ok := existingNamespaces[namespace]; !ok {
		glog.Infof("Creating new namespace %s", namespace)
		k8sNamespaces := k8sClient.CoreV1().Namespaces()
		_, err := k8sNamespaces.Create(&v1.Namespace{
			ObjectMeta: v1.ObjectMeta{
				Name: namespace,
			},
		})
		if err != nil {
			return fmt.Errorf("Unable to create namespace '%s': %s", namespace, err.Error())
		}
		existingNamespaces[namespace] = true
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) createSecrets(k8sClient *k8s.Clientset, existingNamespaces map[string]bool) error {
	secrets := k8sDeployment.DeployedCluster.Deployment.KubernetesDeployment.Secrets
	if len(secrets) == 0 {
		return nil
	}

	for _, secret := range secrets {
		namespace := getNamespace(secret.ObjectMeta)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return fmt.Errorf("Unable to create namespace %s: %s", namespace, err.Error())
		}

		k8sSecret := k8sClient.CoreV1().Secrets(namespace)
		if _, err := k8sSecret.Create(&secret); err != nil {
			return fmt.Errorf("Unable to create secret %s: %s", secret.Name, err.Error())
		}
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) createServiceForDeployment(namespace string, family string, k8sClient *k8s.Clientset, task apis.KubernetesTask, container v1.Container) error {
	log := k8sDeployment.DeployedCluster.Logger

	if len(container.Ports) == 0 {
		return nil
	}

	service := k8sClient.CoreV1().Services(namespace)
	serviceName := family
	if !strings.HasPrefix(family, serviceName) {
		serviceName = serviceName + "-" + container.Name
	}
	labels := map[string]string{"app": family}
	servicePorts := []v1.ServicePort{}
	for i, port := range container.Ports {
		newPort := v1.ServicePort{
			Port:       port.HostPort,
			TargetPort: intstr.FromInt(int(port.ContainerPort)),
			Name:       "port" + strconv.Itoa(i),
		}
		servicePorts = append(servicePorts, newPort)
	}

	internalService := &v1.Service{
		ObjectMeta: v1.ObjectMeta{
			Name:      serviceName,
			Labels:    labels,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Type:     v1.ServiceTypeClusterIP,
			Ports:    servicePorts,
			Selector: labels,
		},
	}
	_, err := service.Create(internalService)
	if err != nil {
		return fmt.Errorf("Unable to create service %s: %s", serviceName, err)
	}
	log.Infof("Created %s internal service", serviceName)

	// Check the type of each port opened by the container; create a loadbalancer service to expose the public port
	if task.PortTypes == nil || len(task.PortTypes) == 0 {
		return nil
	}

	for i, portType := range task.PortTypes {
		if portType != publicPortType {
			log.Infof("Skipping creating public endpoint for service %s as it's marked as private", serviceName)
			continue
		}
		// public port
		publicServiceName := serviceName + "-public" + servicePorts[i].Name
		publicService := &v1.Service{
			ObjectMeta: v1.ObjectMeta{
				Name:      publicServiceName,
				Labels:    labels,
				Namespace: namespace,
			},
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
				Ports: []v1.ServicePort{
					v1.ServicePort{
						Port:       servicePorts[i].Port,
						TargetPort: servicePorts[i].TargetPort,
						Name:       "public-" + servicePorts[i].Name,
					},
				},
				Selector: labels,
			},
		}
		_, err := service.Create(publicService)
		if err != nil {
			return fmt.Errorf("Unable to create public service %s: %s", publicServiceName, err)
		}

		log.Infof("Created a public service %s with port %d", publicServiceName, servicePorts[i].Port)
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) deployServices(k8sClient *k8s.Clientset, existingNamespaces map[string]bool) error {
	log := k8sDeployment.DeployedCluster.Logger
	if k8sDeployment.KubeConfig == nil {
		return errors.New("Unable to find kube config in deployment")
	}

	tasks := map[string]apis.KubernetesTask{}
	for _, task := range k8sDeployment.DeployedCluster.Deployment.KubernetesDeployment.Kubernetes {
		tasks[task.Family] = task
	}

	taskCount := map[string]int{}
	for _, mapping := range k8sDeployment.DeployedCluster.Deployment.NodeMapping {
		log.Infof("Deploying task %s with mapping %d", mapping.Task, mapping.Id)

		task, ok := tasks[mapping.Task]
		if !ok {
			return fmt.Errorf("Unable to find task %s in task definitions", mapping.Task)
		}

		deploySpec := task.Deployment
		if deploySpec == nil {
			return fmt.Errorf("Unable to find deployment in task %s", mapping.Task)
		}
		family := task.Family
		namespace := getNamespace(deploySpec.ObjectMeta)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return err
		}

		originalFamily := family
		count, ok := taskCount[family]

		if !ok {
			count = 1
			deploySpec.Name = originalFamily
			deploySpec.Labels["app"] = originalFamily
			deploySpec.Spec.Template.Labels["app"] = originalFamily
		} else {
			// Update deploy spec to reflect multiple count of the same task
			count += 1
			family = family + "-" + strconv.Itoa(count)
			deploySpec.Name = family
			deploySpec.Labels["app"] = family
			deploySpec.Spec.Template.Labels["app"] = family
		}

		if deploySpec.Spec.Selector != nil {
			deploySpec.Spec.Selector.MatchLabels = deploySpec.Spec.Template.Labels
		}

		taskCount[originalFamily] = count

		// Assigning Pods to Nodes
		nodeSelector := map[string]string{}
		log.Infof("Selecting node %d for deployment %s", mapping.Id, family)
		nodeSelector["hyperpilot/node-id"] = strconv.Itoa(mapping.Id)

		deploySpec.Spec.Template.Spec.NodeSelector = nodeSelector

		// Create service for each container that opens a port
		for _, container := range deploySpec.Spec.Template.Spec.Containers {
			err := k8sDeployment.createServiceForDeployment(namespace, family, k8sClient, task, container)
			if err != nil {
				return fmt.Errorf("Unable to create service for deployment %s: %s", family, err.Error())
			}
		}

		deploy := k8sClient.Extensions().Deployments(namespace)
		_, err := deploy.Create(deploySpec)
		if err != nil {
			return fmt.Errorf("Unabel to create k8s deployment: %s", err)
		}
		log.Infof("%s deployment created", family)
	}

	for _, task := range k8sDeployment.DeployedCluster.Deployment.KubernetesDeployment.Kubernetes {
		if task.DaemonSet == nil {
			continue
		}

		if task.Deployment != nil {
			return fmt.Errorf("Cannot assign both daemonset and deployment to the same task: %s", task.Family)
		}

		daemonSet := task.DaemonSet
		namespace := getNamespace(daemonSet.ObjectMeta)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return err
		}

		daemonSets := k8sClient.Extensions().DaemonSets(namespace)
		log.Infof("Creating daemonset %s", task.Family)
		if _, err := daemonSets.Create(daemonSet); err != nil {
			return fmt.Errorf("Unable to create daemonset %s: %s", task.Family, err.Error())
		}
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) recordPublicEndpoints(k8sClient *k8s.Clientset) {
	log := k8sDeployment.DeployedCluster.Logger
	allNamespaces := k8sDeployment.getAllDeployedNamespaces()
	endpoints := map[string]string{}
	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		tagElbFunc := func() {
			allElbsTagged := true
			for _, namespace := range allNamespaces {
				services := k8sClient.CoreV1().Services(namespace)
				serviceLists, listError := services.List(v1.ListOptions{})
				if listError != nil {
					log.Warningf("Unable to list services for namespace '%s': %s", namespace, listError.Error())
					return
				}
				for _, service := range serviceLists.Items {
					serviceName := service.GetObjectMeta().GetName()
					if strings.Index(serviceName, "-public") != -1 {
						if len(service.Status.LoadBalancer.Ingress) > 0 {
							hostname := service.Status.LoadBalancer.Ingress[0].Hostname
							port := service.Spec.Ports[0].Port
							endpoints[serviceName] = hostname + ":" + strconv.FormatInt(int64(port), 10)
						} else {
							allElbsTagged = false
							break
						}
					}
				}

				if allElbsTagged {
					c <- true
				} else {
					time.Sleep(time.Second * 2)
				}
			}
		}

		for {
			select {
			case <-quit:
				return
			default:
				tagElbFunc()
			}
		}
	}()

	select {
	case <-c:
		log.Info("All public endpoints recorded.")
		k8sDeployment.Endpoints = endpoints
	case <-time.After(time.Duration(2) * time.Minute):
		quit <- true
		log.Warning("Timed out waiting for AWS ELB to be ready.")
	}
}

// DownloadKubeConfig is Use SSHProxyCommand download k8s master node's kubeconfig
func (k8sDeployment *KubernetesDeployment) downloadKubeConfig() error {
	deployedCluster := k8sDeployment.DeployedCluster
	baseDir := deployedCluster.Deployment.Name + "_kubeconfig"
	basePath := "/tmp/" + baseDir
	kubeconfigFilePath := basePath + "/kubeconfig"

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}

	os.Remove(kubeconfigFilePath)

	clientConfig, clientConfigErr := k8sDeployment.DeployedCluster.SshConfig("ubuntu")
	if clientConfigErr != nil {
		return errors.New("Unable to create ssh config: " + clientConfigErr.Error())
	}

	sshClient := common.NewSshClient(k8sDeployment.MasterIp+":22", clientConfig, k8sDeployment.BastionIp+":22")
	if err := sshClient.CopyRemoteFileToLocal("/home/ubuntu/kubeconfig", kubeconfigFilePath); err != nil {
		return errors.New("Unable to copy kubeconfig file to local: " + err.Error())
	}

	k8sDeployment.KubeConfigPath = kubeconfigFilePath
	return nil
}

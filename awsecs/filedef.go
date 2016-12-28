package awsecs

var trustDocument = `{
    "Version": "2012-10-17",
    "Statement": {
        "Effect": "Allow",
        "Principal": {"Service": "ec2.amazonaws.com"},
        "Action": "sts:AssumeRole"
    }
}`

// aws-region:weave-ami
var amiCollection = map[string]string{
	"us-east-1":      "ami-c63709d1",
	"us-east-2":      "ami-4788d222",
	"us-west-1":      "ami-28e7b348",
	"us-west-2":      "ami-c62f81a6",
	"eu-west-1":      "ami-25adf356",
	"eu-central-1":   "ami-7fd31410",
	"ap-northeast-1": "ami-cb1eafaa",
	"ap-southeast-1": "ami-35822f56",
	"ap-southeast-2": "ami-06300965",
}

var defaultRolePolicy = `{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "ecs:CreateCluster",
                "ecs:DeregisterContainerInstance",
                "ecs:DiscoverPollEndpoint",
                "ecs:Poll",
                "ecs:RegisterContainerInstance",
                "ecs:Submit*",
                "ecs:ListClusters",
                "ecs:ListContainerInstances",
                "ecs:DescribeContainerInstances",
                "ecs:StartTelemetrySession",
                "ecs:ListServices",
                "ecs:DescribeTasks",
                "ecs:DescribeServices",
                "ec2:DescribeInstances",
                "ec2:DescribeTags",
                "autoscaling:DescribeAutoScalingInstances"
            ],
            "Resource": [
                "*"
            ]
        }
    ]
}`

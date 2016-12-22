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
// FIXME fild in records of weave ami
var amiCollection = map[string]string{}

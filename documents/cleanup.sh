NAME=$1
EC2_ID=$(aws ec2 describe-instances | jq .Reservations[].Instances[].InstanceId | cut -d"\"" -f2)
aws ec2 terminate-instances --instance-id $EC2_ID
aws ec2 wait instance-terminated --instance-ids $EC2_ID
aws ec2 delete-key-pair --key-name $NAME-key
aws iam remove-role-from-instance-profile --instance-profile-name $NAME --role-name $NAME-role
aws ec2 describe-subnets | jq .Subnets[0].SubnetId | cut -d"\"" -f2 | xargs -I{} aws ec2 delete-subnet --subnet-id {}
VPC_ID=`aws ec2 describe-vpcs | jq .Vpcs[0].VpcId | cut -d"\"" -f2`
aws ec2 describe-security-groups | jq ".SecurityGroups[] | select(.VpcId == \"$VPC_ID\") | select(.GroupName == \"$NAME\")" | jq .GroupId | cut -d"\"" -f2 | xargs -I{} aws ec2 delete-security-group --group-id {}
GW_ID=$(aws ec2 describe-internet-gateways | jq .InternetGateways[].InternetGatewayId | cut -d"\"" -f2)
aws ec2 detach-internet-gateway --internet-gateway-id $GW_ID --vpc-id $VPC_ID
aws ec2 delete-internet-gateway --internet-gateway-id $GW_ID
aws ec2 delete-vpc --vpc-id $VPC_ID
aws iam delete-role-policy --role-name $NAME-role --policy-name $NAME-policy
aws iam delete-role --role-name $NAME-role
aws iam delete-instance-profile --instance-profile-name $NAME

#!/bin/bash

echo "Building deployer..."
go build

echo "Building image and pushing to AWS ECR"
aws ecr get-login | sudo bash

sudo docker build . -t 416594702355.dkr.ecr.us-east-1.amazonaws.com/deployer

sudo docker push 416594702355.dkr.ecr.us-east-1.amazonaws.com/deployer

# Example

## Recommendations

  * Postman for API development

## Steps

1. Start the server

`./deployer --config [PATH OF YOUR CONFIG]`

2. Create a deployment by a HTTP request.
```
POST /v1/deployments

body:
{
  "name":"2",
  "roleName": "2-role",
  "scale": 7,
  "taskDefinitions": [{
    "containerDefinitions":[{}],
    "family": "snap"
  }],
  "clusterDefinition": {
    "nodes": [{
      id": 2,
      "instanceType": "t2",
      "imageId": "AMI-XXXXX"
    }]
  },
  "nodeMapping": [{
    "id": 1,
    "task": "snap:alpine"
  }],
  "iamRole": {
    "roleName": "weave-demo-role",
    "policyName": "weave-demo-policy",
    "policyDocument": "weave-demo-policy"
  }
}
format: application/json
```
[![Run in Postman](https://run.pstmn.io/button.svg)](https://app.getpostman.com/run-collection/6dab7aa89992546aeea7)

# Deployer

# Development

* Required
    * Unix-like environment

```{shell}
# init
make init

# test
make test

# build
make build

# for developer
make dev-test
```

## TODO

* [X]According to the deployment script of microservices-demo, implement the deployer
* [ ]Sent a request to BloxAws to create snap daemon
* [ ]Sent a request to AWS ECS to create a influxdb daemon
* [ ]Design the error log
* [ ]Implement the worker of createDeployment and deleteDeployment

#!/usr/bin/env bash

kubectl get daemonsets | tail -n +2 | cut -d" " -f1 | xargs kubectl delete daemonsets
kubectl get daemonsets -n hyperpilot | tail -n +2 | cut -d" " -f1 | xargs kubectl delete -n hyperpilot daemonsets

kubectl get deployments | tail -n +2 | cut -d" " -f1 | xargs kubectl delete deployments
kubectl get deployments -n hyperpilot | tail -n +2 | cut -d" " -f1 | xargs kubectl delete -n hyperpilot deployments

kubectl get services | tail -n +2 | cut -d" " -f1 | xargs kubectl delete services
kubectl get services -n hyperpilot | tail -n +2 | cut -d" " -f1 | xargs kubectl delete -n hyperpilot services

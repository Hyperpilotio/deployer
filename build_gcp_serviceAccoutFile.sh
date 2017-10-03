#!/bin/bash

# Use GCP web console or gcloud create projectId before run
# System will auto create 'compute Engine default service account' when you first click 'Container Engine' from GCP web console
# gcloud init
echo "Building GCP Service account JSON File"
gcloud iam service-accounts keys create \
    ~/gcpServiceAccount.json \
    --iam-account $(gcloud iam service-accounts list | grep "Compute Engine default service account" | awk '{print $6}')

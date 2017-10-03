package gcpgke

var kubeconfigYamlTemplate = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: $CA_CERT
    server: https://$KUBERNETES_MASTER_NAME
  name: gke_$PROJECT_ID_$ZONE_$CLUSTER_ID
contexts:
- context:
    cluster: gke_$PROJECT_ID_$ZONE_$CLUSTER_ID
    user: gke_$PROJECT_ID_$ZONE_$CLUSTER_ID
  name: gke_$PROJECT_ID_$ZONE_$CLUSTER_ID
current-context: gke_$PROJECT_ID_$ZONE_$CLUSTER_ID
kind: Config
preferences: {}
users:
- name: gke_$PROJECT_ID_$ZONE_$CLUSTER_ID
  user:
    client-certificate-data: $KUBELET_CERT
    client-key-data: $KUBELET_KEY
`

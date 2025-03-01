# Trivy Adapter
The image vulnerability adapter runs an image vulnerability scanner tool called [trivy](https://github.com/aquasecurity/trivy) to scan a given kubernetes pod or workload to produces a namespace policy report based on the [Policy Report Custom Resource Definition](https://github.com/kubernetes-sigs/wg-policy-prototypes/tree/master/policy-report)

## Installing
Build the trivy-adapter binary: `make imgvuln`

Type `trivy-adapter` command in your terminal to confirm if the image vulnerability adapter is installed already

**Note**:
* If it brings an error, the build is completed in the bin folder in the project, you just need to move the binary to your `/usr/local/bin` directory 
* You can remove or delete from your `urs/local/bin` directory.

## Running

**Prerequisites**: 
* Kubernetes Cluster: To run the Kubernetes cluster locally, tools like [kind](https://kind.sigs.k8s.io/) or [minikube](https://minikube.sigs.k8s.io/docs/start/) can be used. Here are the steps to run the image vulnerability adapter with getting a `kind` cluster up and running.

### Steps

#### Common steps
```sh
# 1. Create a Kubernetes cluster
kind create cluster --name mycluster --image <kindest/node:stpecify version tag>

# 2. Create a policy report CustomResourceDefinition
kubectl create -f kubernetes/crd/v1alpha2/wgpolicyk8s.io_policyreports.yaml

# 3. Create a vulnerability report CustomResourceDefinition
trivy-adapter init

# 4. Create a pod of image openzipkin/zipkin:latest called zipkin
kubectl create deployment zipkin --image openzipkin/zipkin:latest
```
**Note**:
* You can also deploy your pod that has more than one containers.

#### Steps to run image vulnerability adapter on your pod or workload
```sh
# 5. Check your pods and namespaces
kubectl get pod --all-namespaces

# 6. Scan default pods
trivy-adapter scan policyreports zipkin-uewcde32cs9-ui

# 7. Check policyreports created through the custom resource
kubectl get policyreports
```
**Note**:
* You should add Flags `-yaml` or `json` to view the full policy report of each pod.
```sh
kubectl get policyreports pod-zipkin-uewcde32cs9-ui -o yaml
```
#### Road Maps:
* CronJob for periodic scan on the pods or workloads to create and update the namespace policy report.
* Extend scan to a cluster wide scan to scan all workloads and pods to create or update the cluster policy report periodically using CronJob.
# native-kubelet
native-kubelet

```shell
apt install libbtrfs-dev -y
apt install golang-github-proglottis-gpgme-dev -y
sudo apt-get install -y pkg-config
apt install libdevmapper-dev -y

kind create cluster --config kind-config-1.yaml --name c1

export VKUBELET_POD_IP=192.168.212.222
export APISERVER_CERT_LOCATION=/mnt/d/code/cluster-kubelet/hack/skaffold/virtual-kubelet/vkubelet-mock-0-crt.pem
export APISERVER_KEY_LOCATION=/mnt/d/code/cluster-kubelet/hack/skaffold/virtual-kubelet/vkubelet-mock-0-key.pem
export JAEGER_ENDPOINT=127.0.0.1:14268

/mnt/d/code/native-kubelet/bin/virtual-kubelet --provider-config=/mnt/d/code/native-kubelet/example/config.json --kubeconfig=/root/.kube/config --nodename vkubelet-mock-0 --provider native-kubelet --startup-timeout 10s --klog.v "2" --klog.logtostderr --log-level debug --trace-exporter=jaeger --trace-sample-rate=always
```
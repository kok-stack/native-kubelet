# export POD_IP=10.0.2.15
etcdName=$(hostname)
discovery=$(curl https://discovery.etcd.io/new?size=1)
./etcd --name ${etcdName} --initial-advertise-peer-urls http://${POD_IP}:2380 \
  --listen-peer-urls http://${POD_IP}:2380 \
  --listen-client-urls http://${POD_IP}:2379,http://127.0.0.1:2379 \
  --advertise-client-urls http://${POD_IP}:2379 \
  --discovery ${discovery} \
  --initial-cluster-state new
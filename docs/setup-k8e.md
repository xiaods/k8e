# 使用k8e快速部署Kubernetes集群服务
作为经常需要使用Kubernetes集群做很多技术验证的场景，我们需要快速构建集群。当前能快速部署Kubernetes 集群的方式有很多种，官方有kubeadm工具首当其冲，社区有sealos作为一键部署的最佳方案，都非常流行。但是我给大家推荐的这种方式，是基于二进制方式的部署，踢出docker 镜像的依赖，并且把Kubernetes相关的生态管理工具都集成到一个二进制包中，通过软链接暴露，让环境依赖更少。k8e是基于我对Kubernetes最佳部署实践的理解，剔除不必要的特性，让你更方便的操作理解Kubernetes，并且和Kubernetes的行为一致，你可以自由扩展。

启动k8e，你可以自己放一台机器做试验就可以，4Core/8G RAM是最小标配。有很多朋友还想安装集群高可用模式，那么就需要三台起步。操作部署步骤如下：

1. 下载一键安装工具k8e
```
mkdir -p /opt/k8e && cd /opt/k8e

curl https://gitreleases.dev/gh/xiaods/k8e/latest/k8e -o k8e

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-bootstrap.sh -o start-bootstrap.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-server.sh -o start-server.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-agent.sh -o start-agent.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/stop-k8e.sh -o stop-k8e.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/setup-k8s-tools.sh -o setup-k8s-tools.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-uninstall.sh -o k8e-uninstall.sh
```
2. 启动集群过程：
* 第一台，属于引导服务(注意：第一台主机IP就是**api-server**的IP)：bash start-bootstrap.sh
* 第2台到N+1台主控节点，必须是奇数，遵循**CAP原理**(注意：启动前修改api-server的IP，指向第一台主机IP)：bash start-server.sh
* 第1台到N台工作负载节点，遵循**CAP原理**(注意：启动前修改api-server的IP，指向第一台主机IP)：bash start-agent.sh
* 停掉K8：bash stop-k8e.sh
* 加载K8s工具链（kubectl ctr crictl）：bash setup-k8s-tools.sh

***Note*** : k8e 内置一个同步 api-server ip的功能，同步后三台主机，宕机任何一台，集群还是HA高可用的。

默认kubeconfig放在 /etc/k8e/k8e/k8e.yaml中。


3. 你有三台Server，就会有3个api-server的入口，一般我们期望加一个haproxy来汇聚api入口。这个可以通过kube-vip来实现。下载kubeconfig文件，就可以远程管理。注意这里对于VIP的IP，我们需要配置一个弹性IP来作为api-server的唯一入口IP，需要启动k8e时告诉它生成正确的证书。
```
--tls-san value   (listener) Add additional hostname or IP as a Subject Alternative Name in the TLS cert
```
bootstrap server和其它server都需要配置样例的参数：
```
k8e server --tls-san 192.168.1.1 
```
4. 加载其它容器网络，和标准k8s一样，只是需要静止掉默认的 flannel 网络。比如支持cilium网络：

bootstrap server(172.25.1.55): 
```
K8E_NODE_NAME=k8e-55  K8E_TOKEN=ilovek8e /opt/k8e/k8e server --flannel-backend=none --cluster-init --disable servicelb,traefik >> k8e.log 2>&1 &
```
server 2(172.25.1.56):
```
K8E_NODE_NAME=k8e-56 K8E_TOKEN=ilovek8e /opt/k8e/k8e server --server https://172.25.1.55:6443 --flannel-backend=none --disable servicelb,traefik >> k8e.log 2>&1 &
```
server 3(172.25.1.57):
```
K8E_NODE_NAME=k8e-57 K8E_TOKEN=ilovek8e /opt/k8e/k8e server --server https://172.25.1.57:6443 --flannel-backend=none --disable servicelb,traefik >> k8e.log 2>&1 &
```

安装cilium
```
helm install cilium cilium/cilium --version 1.9.5 --set global.etcd.enabled=true --set global.etcd.ssl=true  --set global.prometheus.enabled=true --set global.etcd.endpoints[0]=https://172.25.1.55:2379 --namespace kube-system
```

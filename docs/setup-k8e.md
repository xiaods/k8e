# 使用k8e快速部署Kubernetes集群服务
作为YAML工程师，经常需要使用Kubernetes集群来验证很多技术化场景，如何快速搭建一套完整标准化的集群至关重要。罗列当前能快速部署Kubernetes 集群的工具有很多种，例如官方首当其冲有**kubeadm**工具，云原生社区有**sealos**作为一键部署的最佳方案，熟悉起来后部署都非常快。但是你是否考虑过并不是每一个YAML工程师都需要非常了解集群组件的搭配。这里，我给大家推荐的工具是基于单个文件的免配置的部署方式，对比kubeadm和sealos方案，去掉了对 Kubernetes 官方组件镜像的依赖，并且把Kubernetes相关的核心扩展推荐组件也都集成到这个二进制包中，通过软链接暴露，让环境依赖更少，这个安装工具就是**k8e**(可以叫 'kuber easy' 或 K8易) 。k8e是基于当前主流上游Kubernetes发行版 k3s做的优化封装和裁剪。去掉对IoT的依赖，目标就是做最好的服务器版本的发行版本。并且和上游保持一致，可以自由扩展。

**K8e v1**架构图：

![k8e-arch](./k8e-arch.png)



启动k8e，你可以自己放一台机器做试验就可以，**4Core/8G RAM**是最小标配。有很多朋友还想安装集群高可用模式，那么就需要三台起步。操作部署步骤如下：

1. ### 下载一键安装工具k8e
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
4. k8e默认支持flannel网络，更换为eBPF/cilium网络，可如下配置启动加载：

注意主机系统必须满足：**Linux kernel >= 4.9.17**
升级内核

下载内核升级包：http://elrepo.reloumirrors.net/kernel/el7/x86_64/RPMS/
```
rpm -ivh kernel-ml-5.11.6-1.el7.elrepo.x86_64.rpm 
rpm -ivh kernel-ml-devel-5.11.6-1.el7.elrepo.x86_64.rpm 
rpm -ivh kernel-ml-headers-5.11.6-1.el7.elrepo.x86_64.rpm
grub2-set-default 0
reboot
[root@k8e-55 k8e]# uname -r
5.11.6-1.el7.elrepo.x86_64
```
bootstrap server(172.25.1.55): 
```
K8E_NODE_NAME=k8e-55  K8E_TOKEN=ilovek8e /opt/k8e/k8e server --flannel-backend=none --disable-kube-proxy=true --cluster-init --disable servicelb,traefik >> k8e.log 2>&1 &
```
server 2(172.25.1.56):
```
K8E_NODE_NAME=k8e-56 K8E_TOKEN=ilovek8e /opt/k8e/k8e server --server https://172.25.1.55:6443 --flannel-backend=none --disable servicelb,traefik --disable-kube-proxy=true >> k8e.log 2>&1 &
```
server 3(172.25.1.57):
```
K8E_NODE_NAME=k8e-57 K8E_TOKEN=ilovek8e /opt/k8e/k8e server --server https://172.25.1.55:6443 --flannel-backend=none --disable servicelb,traefik --disable-kube-proxy=true >> k8e.log 2>&1 &
```
挂载BPF文件系统
```
sudo mount bpffs -t bpf /sys/fs/bpf
```
添加 helm cilium repo
```
helm repo add cilium https://helm.cilium.io/
```
创建 etcd ssl 证书
```
kubectl create secret generic -n kube-system cilium-etcd-secrets \
                        --from-file=etcd-client-ca.crt=/var/lib/k8e/k8e/server/tls/etcd/server-ca.crt \
                        --from-file=etcd-client.key=/var/lib/k8e/k8e/server/tls/etcd/client.key \
                        --from-file=etcd-client.crt=/var/lib/k8e/k8e/server/tls/etcd/client.crt
```
安装cilium
```
helm install cilium cilium/cilium --version 1.9.5 --set nodeinit.enabled=true \
                                                  --set nodeinit.restartPods=true \
                                                  --set tunnel=disabled \
                                                  --set bpf.masquerade=true \
                                                  --set bpf.clockProbe=true \
                                                  --set bpf.waitForMount=true \
                                                  --set bpf.preallocateMaps=true \
                                                  --set bpf.tproxy=true \
                                                  --set bpf.hostRouting=false \
                                                  --set autoDirectNodeRoutes=true \
                                                  --set localRedirectPolicy=true \
                                                  --set enableK8sEndpointSlice=true \
                                                  --set wellKnownIdentities.enabled=true \
                                                  --set sockops.enabled=true \
                                                  --set endpointRoutes.enabled=false \
                                                  --set nativeRoutingCIDR=10.43.0.0/28 \
                                                  --set enable-node-port=true \
                                                  --set hostServices.enabled=true \
                                                  --set nodePort.enabled=true \
                                                  --set hostPort.enabled=true \
                                                  --set kubeProxyReplacement=strict \
                                                  --set loadBalancer.mode=dsr \
                                                  --set k8sServiceHost=172.25.1.55 \
                                                  --set k8sServicePort=6443 \
                                                  --set global.etcd.endpoints[0]=https://172.25.1.55:2379 \
                                                  --namespace kube-system
```
**安装成功**：

![k8e-cilium](./k8e-cilium.png)

安装Hubble
```
helm upgrade cilium cilium/cilium --version 1.9.5 \
   --namespace kube-system \
   --reuse-values \
   --set hubble.listenAddress=":4244" \
   --set hubble.relay.enabled=true \
   --set hubble.ui.enabled=true
```
如果需要通过nodeport的方式访问，可以创建如下service，访问http://{$Externap_IP}:32000即可看到相关的策略
```yaml
apiVersion: v1
kind: Service
metadata:
  name: hubble-ui-node
  namespace: kube-system
spec:
  ports:
  - name: http
    port: 8081
    protocol: TCP
    targetPort: 8081
    nodePort: 32000
  selector:
    k8s-app: hubble-ui
  sessionAffinity: None
  type: NodePort
  ```

  ![k8e-hubble](./k8e-hubble.png)



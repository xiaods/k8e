# 使用k8e快速部署Kubernetes + Calico集群服务

节点资源列表
172.25.1.55
172.25.1.56
172.25.1.58

### 升级内核

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

### 安装k8s集群

bootstrap server(172.25.1.55): 
```
K8E_NODE_NAME=k8e-55  K8E_TOKEN=ilovek8e /opt/k8e/k8e server --flannel-backend=none --disable-kube-proxy=true --cluster-init --disable servicelb >> k8e.log 2>&1 &
```
server 2(172.25.1.56):
```
K8E_NODE_NAME=k8e-56 K8E_TOKEN=ilovek8e /opt/k8e/k8e server --server https://172.25.1.55:6443 --flannel-backend=none --disable servicelb --disable-kube-proxy=true >> k8e.log 2>&1 &
```
server 3(172.25.1.57):
```
K8E_NODE_NAME=k8e-57 K8E_TOKEN=ilovek8e /opt/k8e/k8e server --server https://172.25.1.55:6443 --flannel-backend=none --disable servicelb --disable-kube-proxy=true >> k8e.log 2>&1 &
```

### 网络堆栈Calico集成

通过下载Calico manifests并修改它们来启动：
```
wget https://docs.projectcalico.org/manifests/tigera-operator.yaml

wget https://docs.projectcalico.org/manifests/custom-resources.yaml

```
打开custom-resources.yaml文件并更改CIDR到与上文提到的相同的IP地址段。

应用两个manifest为K8e集群配置Calico网络
```
kubectl create -f tigera-operator.yaml
kubectl create -f custom-resources.yaml
```
几分钟内，集群状态将变为ready

```
kubectl edit cm cni-config -n calico-system
```

改变以下所示的值，启用IP转发：
```
"container_settings": {
              "allow_ip_forwarding": true
          }
```

验证Calico是否启动并使用以下命令运行：
```
kubectl get pods -n calico-system
```


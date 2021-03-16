# 使用k8e快速部署Kubernetes集群服务
作为经常需要使用Kubernetes集群做很多技术验证的场景，我们需要快速构建集群。当前能快速部署Kubernetes 集群的方式有很多种，官方有kubeadm工具首当其冲，社区有sealos作为一键部署的最佳方案，都非常流行。但是我给大家推荐的这种方式，是基于二进制方式的部署，踢出docker 镜像的依赖，并且把Kubernetes相关的生态管理工具都集成到一个二进制包中，通过软链接暴露，让环境依赖更少。k8e是基于我对Kubernetes最佳部署实践的理解，剔除不必要的特性，让你更方便的操作理解Kubernetes，并且和Kubernetes的行为一致，你可以自由扩展。

启动k8e，你可以自己放一台机器做试验就可以，4Core/8G RAM是最小标配。有很多朋友还想安装集群高可用模式，那么就需要三台起步。操作部署步骤如下：

1. 下载一键安装工具k8e
```
mkdir -p /opt/k8e && cd /opt/k8e

curl https://github.com/xiaods/k8e/releases/download/v1.19.8%2Bk8e1/k8e-amd64 -o k8e

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-bootstrap.sh -o start-bootstrap.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-server.sh -o start-server.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-agent.sh -o start-agent.sh

https://raw.githubusercontent.com/xiaods/k8e/master/contrib/stop-k8e.sh -o stop-k8e.sh

https://raw.githubusercontent.com/xiaods/k8e/master/contrib/setup-k8s-tools.sh -o setup-k8s-tools.sh
```
3. 启动集群过程：
* 第一台，属于引导服务(注意：第一台主机IP就是**api-server**的IP)：bash start-bootstrap.sh
* 第2台到N+1台主控节点，必须是奇数，遵循**CAP原理**(注意：启动前修改api-server的IP，指向第一台主机IP)：bash start-server.sh
* 第1台到N台工作负载节点，遵循**CAP原理**(注意：启动前修改api-server的IP，指向第一台主机IP)：bash start-agent.sh
* 停掉K8：bash stop-k8e.sh
* 加载K8s工具链（kubectl ctr crictl）：bash setup-k8s-tools.sh




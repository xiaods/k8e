# 使用k8e快速部署Kubernetes集群服务
1. 下载一键安装工具k8e
```
mkdir -p /opt/k8e && cd /opt/k8e

curl https://github.com/xiaods/k8e/releases/download/v1.19.7%2Bk8e1/k8e-amd64 -o k8e

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-bootstrap.sh -o start-bootstrap.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-server.sh -o start-server.sh

https://raw.githubusercontent.com/xiaods/k8e/master/contrib/stop-k8e.sh -o stop-k8e.sh

https://raw.githubusercontent.com/xiaods/k8e/master/contrib/setup-k8s-tools.sh -o setup-k8s-tools.sh
```
3. 启动集群过程：
* 第一台(注意：第一台主机IP就是api-server的IP)：bash start-bootstrap.sh
* 第二台到N台(注意：启动前修改api-server的IP，指向第一台主机IP)：bash start-server.sh
* 停掉K8：bash stop-k8e.sh
* 加载K8s工具链（kubectl ctr crictl）：bash setup-k8s-tools.sh

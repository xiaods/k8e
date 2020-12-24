terraform {
  backend "local" {
    path = "server.tfstate"
  }
}

locals {
  name                      = var.name
  k8e_cluster_secret        = var.k8e_cluster_secret
  install_k8e_version       = var.k8e_version
  prom_worker_node_count    = var.prom_worker_node_count
  prom_worker_instance_type = var.prom_worker_instance_type
}

provider "aws" {
  region  = "us-east-2"
  profile = "rancher-eng"
}

resource "aws_security_group" "k8e" {
  name   = "${local.name}-sg"
  vpc_id = data.aws_vpc.default.id

  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "TCP"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    from_port   = 6443
    to_port     = 6443
    protocol    = "TCP"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    from_port = 0
    to_port   = 0
    protocol  = "-1"
    self      = true
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_db_instance" "k8e_db" {
  count                = "${var.db_engine == "postgres" || var.db_engine == "mysql" ? 1 : 0 }"
  allocated_storage    = 100 #baseline iops is 300 with gp2
  storage_type         = "gp2"
  engine               = "${var.db_engine}"
  engine_version       = "${var.db_version}"
  instance_class       = "${var.db_instance_type}"
  name                 = "${var.db_name}"
  username             = "${var.db_username}"
  password             = "${var.db_password}"
  skip_final_snapshot  = true
  multi_az             = false
}

resource "aws_instance" "k8e_etcd" {
  count = "${var.etcd_count * (var.db_engine == "etcd" ? 1 * var.server_ha : 0)}"
  instance_type = replace(var.db_instance_type, "/db./", "")
  ami           = data.aws_ami.ubuntu.id
  user_data     = base64encode(templatefile("${path.module}/files/etcd.tmpl",
  {
    extra_ssh_keys = var.extra_ssh_keys,
    db_version = var.db_version
    etcd_count = var.etcd_count
  }))
  security_groups = [
    aws_security_group.k8e.name,
  ]

   root_block_device {
    volume_size = "30"
    volume_type = "gp2"
  }

   tags = {
    Name = "${local.name}-etcd-${count.index}"
  }
}

resource "aws_lb" "k8e-master-nlb" {
  name               = "${local.name}-nlb"
  internal           = false
  load_balancer_type = "network"
  subnets = data.aws_subnet_ids.available.ids
}

resource "aws_route53_record" "www" {
   # currently there is the only way to use nlb dns name in k8e
   # because the real dns name is too long and cause an issue
   zone_id = "${var.zone_id}"
   name = "${var.domain_name}"
   type = "CNAME"
   ttl = "30"
   records = ["${aws_lb.k8e-master-nlb.dns_name}"]
}


resource "aws_lb_target_group" "k8e-master-nlb-tg" {
  name     = "${local.name}-nlb-tg"
  port     = "6443"
  protocol = "TCP"
  vpc_id   = data.aws_vpc.default.id
  deregistration_delay = "300"
  health_check {
    interval = "30"
    port = "6443"
    protocol = "TCP"
    healthy_threshold = "10"
    unhealthy_threshold= "10"
  }
}

resource "aws_lb_listener" "k8e-master-nlb-tg" {
  load_balancer_arn = "${aws_lb.k8e-master-nlb.arn}"
  port              = "6443"
  protocol          = "TCP"
  default_action {
    target_group_arn = "${aws_lb_target_group.k8e-master-nlb-tg.arn}"
    type             = "forward"
  }
}

resource "aws_lb_target_group_attachment" "test" {
  count = "${var.server_count}"
  target_group_arn = "${aws_lb_target_group.k8e-master-nlb-tg.arn}"
  target_id        = "${aws_instance.k8e-server[count.index].id}"
  port             = 6443
}

resource "aws_instance" "k8e-server" {
  count = "${var.server_count}"
  instance_type = var.server_instance_type
  ami           = data.aws_ami.ubuntu.id
  user_data     = base64encode(templatefile("${path.module}/files/server_userdata.tmpl",
  {
    extra_ssh_keys = var.extra_ssh_keys,
    k8e_cluster_secret = local.k8e_cluster_secret,
    install_k8e_version = local.install_k8e_version,
    k8e_server_args = var.k8e_server_args,
    db_engine = var.db_engine,
    db_address = "${var.db_engine == "etcd" ? join(",",aws_instance.k8e_etcd.*.private_ip) : var.db_engine == "embedded-etcd" ? "null" : aws_db_instance.k8e_db[0].address}",
    db_name = var.db_name,
    db_username = var.db_username,
    db_password = var.db_password,
    use_ha = "${var.server_ha == 1 ? "true": "false"}",
    master_index = count.index,
    lb_address = var.domain_name,
    prom_worker_node_count = local.prom_worker_node_count,
    debug = var.debug,
    k8e_cluster_secret = local.k8e_cluster_secret,}))
  security_groups = [
    aws_security_group.k8e.name,
  ]

   root_block_device {
    volume_size = "30"
    volume_type = "gp2"
  }

   tags = {
    Name = "${local.name}-server-${count.index}"
    Role = "master"
    Leader = "${count.index == 0 ? "true" : "false"}"
  }
  provisioner "local-exec" {
      command = "sleep 10"
  }
}

module "k8e-prom-worker-asg" {
  source        = "terraform-aws-modules/autoscaling/aws"
  version       = "3.0.0"
  name          = "${local.name}-prom-worker"
  asg_name      = "${local.name}-prom-worker"
  instance_type = local.prom_worker_instance_type
  image_id      = data.aws_ami.ubuntu.id
  user_data     = base64encode(templatefile("${path.module}/files/worker_userdata.tmpl", { extra_ssh_keys = var.extra_ssh_keys, k8e_url = var.domain_name, k8e_cluster_secret = local.k8e_cluster_secret, install_k8e_version = local.install_k8e_version, k8e_exec = "--node-label prom=true" }))

  desired_capacity    = local.prom_worker_node_count
  health_check_type   = "EC2"
  max_size            = local.prom_worker_node_count
  min_size            = local.prom_worker_node_count
  vpc_zone_identifier = [data.aws_subnet.selected.id]
  spot_price          = "0.340"

  security_groups = [
    aws_security_group.k8e.id,
  ]

  lc_name = "${local.name}-prom-worker"

  root_block_device = [
    {
      volume_size = "30"
      volume_type = "gp2"
    },
  ]
}

resource "null_resource" "run_etcd" {
  count = "${var.db_engine == "etcd" ? 1 : 0}"

  triggers = {
    etcd_instance_ids = "${join(",", aws_instance.k8e_etcd.*.id)}"
  }

  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = "DB_VERSION=${var.db_version} SSH_KEY_PATH=${var.ssh_key_path} PUBLIC_IPS=${join(",",aws_instance.k8e_etcd.*.public_ip)} PRIVATE_IPS=${join(",",aws_instance.k8e_etcd.*.private_ip)} files/etcd_build.sh"
  }
}

resource "null_resource" "get-kubeconfig" {
  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = "until ssh -i ${var.ssh_key_path} ubuntu@${aws_instance.k8e-server[0].public_ip} 'sudo sed \"s/localhost/$var.domain_name}/g;s/127.0.0.1/${var.domain_name}/g\" /etc/k8e/k8e/k8e.yaml' >| ../tests/kubeconfig.yaml; do sleep 5; done"
  }
}

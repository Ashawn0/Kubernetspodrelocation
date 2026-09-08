terraform {
  required_version = ">= 1.5.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

data "aws_availability_zones" "available" {
  state = "available"
}

# Ubuntu 24.04 LTS Noble amd64
data "aws_ami" "ubuntu_noble" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

data "aws_key_pair" "reloc" {
  key_name = var.key_name
}

locals {
  az           = data.aws_availability_zones.available.names[0]
  name_prefix  = var.name_prefix
  cluster_name = "${var.name_prefix}-kubeadm"
}

resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true
  tags = {
    Name                        = "${local.name_prefix}-vpc"
    "reloc-disrupt/cluster"     = local.cluster_name
    "reloc-disrupt/managed-by"  = "terraform"
  }
}

resource "aws_internet_gateway" "igw" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${local.name_prefix}-igw" }
}

# Primary subnet: CNI / pod / SSH / kubelet API path
resource "aws_subnet" "primary" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.0.1.0/24"
  availability_zone       = local.az
  map_public_ip_on_launch = true
  tags = {
    Name                    = "${local.name_prefix}-primary"
    "reloc-disrupt/role"    = "primary"
  }
}

# Registry subnet: worker secondary ENIs only (tc shaping target)
resource "aws_subnet" "registry" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.0.11.0/24"
  availability_zone       = local.az
  map_public_ip_on_launch = false
  tags = {
    Name                    = "${local.name_prefix}-registry"
    "reloc-disrupt/role"    = "registry"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.igw.id
  }
  tags = { Name = "${local.name_prefix}-public-rt" }
}

resource "aws_route_table_association" "primary" {
  subnet_id      = aws_subnet.primary.id
  route_table_id = aws_route_table.public.id
}

# Registry subnet stays VPC-local (no IGW). Reachability is worker↔worker
# via secondary ENIs for shaping isolation tests.
resource "aws_route_table" "registry" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${local.name_prefix}-registry-rt" }
}

resource "aws_route_table_association" "registry" {
  subnet_id      = aws_subnet.registry.id
  route_table_id = aws_route_table.registry.id
}

resource "aws_security_group" "node" {
  name        = "${local.name_prefix}-node"
  description = "reloc-disrupt kubeadm nodes"
  vpc_id      = aws_vpc.main.id

  # SSH + kube-apiserver: operator_cidr only (never 0.0.0.0/0; validated in variables.tf)
  ingress {
    description = "SSH from operator"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.operator_cidr]
  }

  ingress {
    description = "kube-apiserver from operator"
    from_port   = 6443
    to_port     = 6443
    protocol    = "tcp"
    cidr_blocks = [var.operator_cidr]
  }

  # Intra-VPC (kube, Calico, registry-subnet iperf, etc.)
  ingress {
    description = "VPC internal"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = [aws_vpc.main.cidr_block]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = "${local.name_prefix}-node-sg" }
}

resource "aws_instance" "cp" {
  ami                         = data.aws_ami.ubuntu_noble.id
  instance_type               = var.cp_instance_type
  key_name                    = data.aws_key_pair.reloc.key_name
  subnet_id                   = aws_subnet.primary.id
  vpc_security_group_ids      = [aws_security_group.node.id]
  associate_public_ip_address = true
  iam_instance_profile        = aws_iam_instance_profile.node.name

  root_block_device {
    volume_type = "gp3"
    volume_size = 40
    encrypted   = true
  }

  user_data = templatefile("${path.module}/../scripts/cloud-init-cp.yaml", {
    hostname = "${local.name_prefix}-cp1"
  })

  tags = {
    Name                     = "${local.name_prefix}-cp1"
    "reloc-disrupt/role"     = "control-plane"
    "kubernetes.io/hostname" = "${local.name_prefix}-cp1"
  }
}

resource "aws_instance" "worker" {
  count = var.worker_count

  ami                         = data.aws_ami.ubuntu_noble.id
  instance_type               = var.worker_instance_type
  key_name                    = data.aws_key_pair.reloc.key_name
  subnet_id                   = aws_subnet.primary.id
  vpc_security_group_ids      = [aws_security_group.node.id]
  associate_public_ip_address = true
  iam_instance_profile        = aws_iam_instance_profile.node.name

  root_block_device {
    volume_type = "gp3"
    volume_size = 40
    encrypted   = true
  }

  # c6id: local NVMe instance store is attached by the instance type;
  # aws-node-prep.sh formats/mounts it at /mnt/reloc-nvme.

  user_data = templatefile("${path.module}/../scripts/cloud-init-worker.yaml", {
    hostname = "${local.name_prefix}-worker${count.index + 1}"
  })

  tags = {
    Name                     = "${local.name_prefix}-worker${count.index + 1}"
    "reloc-disrupt/role"     = "worker"
    "kubernetes.io/hostname" = "${local.name_prefix}-worker${count.index + 1}"
  }
}

# Secondary ENI per worker in registry subnet (tc shaping target)
resource "aws_network_interface" "registry" {
  count = var.worker_count

  subnet_id         = aws_subnet.registry.id
  security_groups   = [aws_security_group.node.id]
  source_dest_check = false
  description       = "reloc-disrupt registry path for worker${count.index + 1}"

  tags = {
    Name                 = "${local.name_prefix}-worker${count.index + 1}-registry-eni"
    "reloc-disrupt/role" = "registry-eni"
  }
}

resource "aws_network_interface_attachment" "registry" {
  count = var.worker_count

  instance_id          = aws_instance.worker[count.index].id
  network_interface_id = aws_network_interface.registry[count.index].id
  device_index         = 1
}

# Minimal SSM + describe for ops (optional session manager)
resource "aws_iam_role" "node" {
  name = "${local.name_prefix}-node-role"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action    = "sts:AssumeRole"
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "node" {
  name = "${local.name_prefix}-node-profile"
  role = aws_iam_role.node.name
}

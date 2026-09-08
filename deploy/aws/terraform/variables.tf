variable "region" {
  type    = string
  default = "ap-northeast-2"
}

variable "name_prefix" {
  type    = string
  default = "reloc-disrupt"
}

variable "key_name" {
  type    = string
  default = "reloc-disrupt-key"
}

variable "cp_instance_type" {
  type    = string
  default = "m6i.large"
}

variable "worker_instance_type" {
  type    = string
  default = "c6id.xlarge"
}

variable "worker_count" {
  type    = number
  default = 2
}

# REQUIRED. No 0.0.0.0/0 default — kube-apiserver :6443 and SSH use this CIDR.
# Pass your public IP/32 (up.ps1 can auto-detect and write terraform.tfvars).
variable "operator_cidr" {
  type        = string
  description = "Operator public IP as x.x.x.x/32 for SSH (22) and kube-apiserver (6443). Must not be 0.0.0.0/0."

  validation {
    condition     = var.operator_cidr != "0.0.0.0/0" && can(cidrhost(var.operator_cidr, 0))
    error_message = "operator_cidr must be a real CIDR and must not be 0.0.0.0/0 (refusing open kube-apiserver)."
  }
}

output "region" {
  value = var.region
}

output "cp_public_ip" {
  value = aws_instance.cp.public_ip
}

output "cp_private_ip" {
  value = aws_instance.cp.private_ip
}

output "worker_public_ips" {
  value = aws_instance.worker[*].public_ip
}

output "worker_private_ips" {
  value = aws_instance.worker[*].private_ip
}

output "worker_registry_private_ips" {
  value = aws_network_interface.registry[*].private_ip
}

output "worker_instance_ids" {
  value = aws_instance.worker[*].id
}

output "cp_instance_id" {
  value = aws_instance.cp.id
}

output "ssh_key_name" {
  value = var.key_name
}

output "vpc_id" {
  value = aws_vpc.main.id
}

output "primary_subnet_id" {
  value = aws_subnet.primary.id
}

output "registry_subnet_id" {
  value = aws_subnet.registry.id
}

output "kubeconfig_hint" {
  value = "After bootstrap: deploy/aws/.kube/aws.conf (from fetch-kubeconfig.ps1 / up.ps1)"
}

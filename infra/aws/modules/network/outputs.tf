output "vpc_id" {
  description = "VPC ID."
  value       = aws_vpc.main.id
}

output "public_subnet_ids" {
  description = "Public subnet IDs."
  value       = [for subnet in aws_subnet.public : subnet.id]
}

output "private_subnet_ids" {
  description = "Private subnet IDs."
  value       = [for subnet in aws_subnet.private : subnet.id]
}

output "nat_gateway_id" {
  description = "NAT Gateway ID when enabled."
  value       = try(aws_nat_gateway.main[0].id, null)
}

output "s3_gateway_endpoint_id" {
  description = "S3 gateway endpoint ID when enabled."
  value       = try(aws_vpc_endpoint.s3[0].id, null)
}

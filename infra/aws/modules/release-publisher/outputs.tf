output "role_arn" {
  description = "Create-only Platform Artifact publisher role ARN."
  value       = aws_iam_role.platform_publisher.arn
}

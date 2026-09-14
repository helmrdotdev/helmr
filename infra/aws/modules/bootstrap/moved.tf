moved {
  from = aws_iam_role.platform_publisher
  to   = aws_iam_role.platform_publisher[0]
}

moved {
  from = aws_iam_role_policy.platform_publisher
  to   = aws_iam_role_policy.platform_publisher[0]
}

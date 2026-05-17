terraform {
  required_version = ">= 1.10"

  required_providers {
    http = {
      source  = "hashicorp/http"
      version = ">= 3.5"
    }
  }
}

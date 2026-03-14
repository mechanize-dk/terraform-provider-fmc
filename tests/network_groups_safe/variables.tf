variable "use_safe" {
  description = "When true, use fmc_network_groups_safe instead of fmc_network_groups."
  type        = bool
  default     = false
}

variable "network_groups" {
  description = "Map of network group name => single CIDR literal to assign."
  type        = map(object({ literal = string }))
  default     = {}
}

variable "access_rule_groups" {
  description = "Map of access rule name => network group name. Each rule gets that group as its destination."
  type        = map(string)
  default     = {}
}

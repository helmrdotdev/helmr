// Edit this synthetic fixture before preparation/deployment. Never add service keys.
export const configuration = {
  business: {
    domain: "business.private.example", ipv4: "10.92.1.10", ipRange: "10.92.1.0/24",
    path: "/probe", expectedBody: "helmr-private-business-canary\n",
  },
  // Public test CA certificate only. Keep its signing key outside the project.
  testCA: "REPLACE_WITH_TEST_CA_PEM",
  denied: [
    { kind: "control-plane", domain: "management.private.example", ipv4: "10.90.0.10", port: 443 },
    { kind: "execution", domain: "execution.private.example", ipv4: "10.91.0.10", port: 443 },
    { kind: "company-client", domain: "client.private.example", ipv4: "10.92.0.10", port: 443 },
    { kind: "metadata", domain: "metadata.private.example", ipv4: "169.254.169.254", port: 80 },
  ],
  resolverIPv4: "10.91.0.2",
  alternateDNS: { address: "1.1.1.1", query: "example.com" },
  ipv6: "2606:4700:4700::1111",
}

// Private ranges are authority only for the business entry. Mandatory-deny names
// are deliberately listed: a hostname allowance must not grant management access.
export const finiteNetwork = { allowDomains: [
  { domain: configuration.business.domain, ipRanges: [configuration.business.ipRange] },
  ...configuration.denied.map(target => target.domain),
] }

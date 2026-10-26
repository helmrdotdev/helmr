import { image, source, template } from "@helmr/sdk"

// The archive is staged only in the outside-checkout prepared project.
const tools = source.file("cases/network-clients/native-clients.tar.gz")
const base = image("network-client-tools")
  .from("debian@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132")
  .copy(tools, "/tmp/native-clients.tar.gz")
  .run(["sh", "-ceu", "tar -xzf /tmp/native-clients.tar.gz -C /; rm /tmp/native-clients.tar.gz"])
  .env("PATH", "/opt/network-client-tools/node/bin:/opt/network-client-tools/git/bin:/usr/bin:/bin")
  .run(["sh", "-ceu", "test \"$(node --version)\" = v24.22.1; test \"$(npm --version)\" = 11.19.1; test \"$(git --version)\" = 'git version 2.51.2'"])
  .workdir("/workspace")

export const networkClientComputer = template({
  id: "network-client-qualification", image: base,
  resources: { cpu: 1, memory: "1GiB" },
  network: { allowDomains: ["model", "git", "registry", "auth", "cdn", "private"].map(name => ({
    domain: `${name}.network.test`, ipRanges: ["10.60.1.2/32"],
  })) },
})

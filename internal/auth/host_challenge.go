package auth

// hostChallengeContext separates what a worker machine key signs for a host
// credential from anything else it could be asked to sign.
const hostChallengeContext = "helmr-worker-host-credential-v1\n"

// HostChallengeMessage is what a worker host's machine key signs to answer a
// host credential challenge.
func HostChallengeMessage(challenge string) []byte {
	return []byte(hostChallengeContext + challenge)
}

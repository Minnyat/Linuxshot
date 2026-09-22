package upload

import "testing"

func TestCredentialManager_CredentialConstants(t *testing.T) {
	// Verify credential constants have correct prefix
	constants := []string{
		CredR2AccessKeyID,
		CredR2SecretAccessKey,
		CredR2Endpoint,
		CredR2BucketName,
		CredR2PublicURL,
		CredGDriveToken,
		CredGDriveClientID,
		CredGDriveClientSecret,
	}

	prefix := "WinShot_"
	for _, c := range constants {
		if len(c) < len(prefix) || c[:len(prefix)] != prefix {
			t.Errorf("Credential constant %q should have prefix %q", c, prefix)
		}
	}
}

package updater

import "testing"

func TestSelectAssetURL(t *testing.T) {
	assets := []Asset{
		{Name: "linuxshot-amd64-installer.exe", BrowserDownloadURL: "installer"},
		{Name: "linuxshot.exe", BrowserDownloadURL: "portable"},
		{Name: "linuxshot-linux-amd64.tar.gz", BrowserDownloadURL: "tarball"},
		{Name: "linuxshot_1.0.0_amd64.deb", BrowserDownloadURL: "deb"},
		{Name: "LinuxShot-x86_64.AppImage", BrowserDownloadURL: "appimage"},
	}

	tests := []struct {
		name   string
		assets []Asset
		goos   string
		want   string
	}{
		{"linux prefers AppImage", assets, "linux", "appimage"},
		{"linux falls back to deb", assets[:4], "linux", "deb"},
		{"linux falls back to tarball", assets[:3], "linux", "tarball"},
		{"linux ignores windows assets", assets[:2], "linux", ""},
		{"windows picks portable exe", assets, "windows", "portable"},
		{"windows skips installer only", assets[:1], "windows", ""},
		{"unknown OS", assets, "darwin", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectAssetURL(tt.assets, tt.goos); got != tt.want {
				t.Errorf("selectAssetURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

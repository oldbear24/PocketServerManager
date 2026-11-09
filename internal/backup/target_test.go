package backup

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func keys(t *testing.T) (hostKey, privateKey string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic))) + " root@backup-node", string(pem.EncodeToMemory(block))
}

func TestAnS3TargetIsABucketWithItsKeys(t *testing.T) {
	config, creds, err := Check(KindS3, Config{Endpoint: " s3.eu-central-003.backblazeb2.com/ ", Bucket: "homelab-backups", Prefix: "/restic/nas/", Region: "eu-central-003", Path: "/ignored"},
		Credentials{AccessKeyID: "key-id", SecretAccessKey: "secret-key", Password: "dropped"})
	if err != nil {
		t.Fatal(err)
	}
	if config.Endpoint != "https://s3.eu-central-003.backblazeb2.com" || config.Prefix != "restic/nas" || config.Path != "" || creds.Password != "" {
		t.Errorf("%+v %+v", config, creds)
	}
	repo, err := Locate(KindS3, config, creds)
	if err != nil {
		t.Fatal(err)
	}
	if repo.URL != "s3:https://s3.eu-central-003.backblazeb2.com/homelab-backups/restic/nas" || repo.Env["AWS_ACCESS_KEY_ID"] != "key-id" ||
		repo.Env["AWS_SECRET_ACCESS_KEY"] != "secret-key" || repo.Env["AWS_DEFAULT_REGION"] != "eu-central-003" || len(repo.Options) != 0 {
		t.Errorf("%+v", repo)
	}
	if strings.Contains(repo.URL, "secret") {
		t.Error("a secret is in the URL")
	}
	if where := Where(KindS3, config); where != "s3:homelab-backups/restic/nas @ s3.eu-central-003.backblazeb2.com" {
		t.Errorf("where: %q", where)
	}

	// MinIO on the LAN, path-style
	config, creds, err = Check(KindS3, Config{Endpoint: "http://192.168.1.5:9000", Bucket: "restic", PathStyle: true}, Credentials{AccessKeyID: "a", SecretAccessKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	repo, _ = Locate(KindS3, config, creds)
	if repo.URL != "s3:http://192.168.1.5:9000/restic" || repo.Options["s3.bucket-lookup"] != "path" {
		t.Errorf("%+v", repo)
	}

	for name, bad := range map[string]Config{
		"endpoint with the bucket": {Endpoint: "https://s3.example.com/bucket", Bucket: "restic"},
		"ftp endpoint":             {Endpoint: "ftp://s3.example.com", Bucket: "restic"},
		"user in endpoint":         {Endpoint: "https://k:s@s3.example.com", Bucket: "restic"},
		"upper-case bucket":        {Endpoint: "s3.example.com", Bucket: "Restic"},
		"short bucket":             {Endpoint: "s3.example.com", Bucket: "ab"},
		"prefix up":                {Endpoint: "s3.example.com", Bucket: "restic", Prefix: "a/../b"},
		"region":                   {Endpoint: "s3.example.com", Bucket: "restic", Region: "EU Central"},
	} {
		if _, _, err := Check(KindS3, bad, Credentials{AccessKeyID: "a", SecretAccessKey: "b"}); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	if _, _, err := Check(KindS3, Config{Endpoint: "s3.example.com", Bucket: "restic"}, Credentials{AccessKeyID: "a"}); err == nil {
		t.Error("a missing secret key was taken")
	}
}

func TestAnSFTPTargetPinsTheServersKey(t *testing.T) {
	hostKey, privateKey := keys(t)
	config, creds, err := Check(KindSFTP, Config{Address: "backup-node:2222", User: "backup", Path: "/srv/restic/", HostKey: hostKey}, Credentials{PrivateKey: privateKey, Password: "dropped"})
	if err != nil {
		t.Fatal(err)
	}
	if config.Path != "/srv/restic" || strings.Count(config.HostKey, " ") != 1 || creds.Password != "" {
		t.Errorf("%+v", config)
	}
	repo, err := Locate(KindSFTP, config, creds)
	if err != nil {
		t.Fatal(err)
	}
	if repo.URL != "sftp://backup@backup-node:2222//srv/restic" || repo.SSHKey != creds.PrivateKey ||
		repo.KnownHosts != "[backup-node]:2222 "+config.HostKey+"\n" {
		t.Errorf("%+v", repo)
	}
	command := repo.Options["sftp.command"]
	for _, want := range []string{"-i " + SSHKeyFile, "UserKnownHostsFile=" + KnownHostsFile, "StrictHostKeyChecking=yes", "BatchMode=yes", "ConnectTimeout=20", "ServerAliveInterval=15", "-p 2222", "backup@backup-node", "-s sftp"} {
		if !strings.Contains(command, want) {
			t.Errorf("the command lacks %q: %s", want, command)
		}
	}
	// an IPv6 address keeps its brackets in the URL, where restic finds the
	// directory after them, and in the place people read
	config, creds, err = Check(KindSFTP, Config{Address: "[2001:db8::7]", User: "backup", Path: "/srv/restic", HostKey: hostKey}, Credentials{PrivateKey: privateKey})
	if err != nil {
		t.Fatal(err)
	}
	repo, _ = Locate(KindSFTP, config, creds)
	if config.Address != "[2001:db8::7]" || repo.URL != "sftp://backup@[2001:db8::7]:22//srv/restic" ||
		Where(KindSFTP, config) != "sftp:backup@[2001:db8::7]:/srv/restic" || !strings.HasPrefix(repo.KnownHosts, "2001:db8::7 ssh-ed25519 ") ||
		!strings.Contains(repo.Options["sftp.command"], "backup@2001:db8::7 -s sftp") {
		t.Errorf("IPv6: %+v %q %q", config, repo.URL, repo.KnownHosts)
	}
	// a directory in the user's home is the URL's path as it is
	config, creds, _ = Check(KindSFTP, Config{Address: "backup-node", User: "backup", Path: "restic", HostKey: hostKey}, Credentials{PrivateKey: privateKey})
	if repo, _ = Locate(KindSFTP, config, creds); repo.URL != "sftp://backup@backup-node:22/restic" {
		t.Errorf("a home directory: %q", repo.URL)
	}

	// port 22 is the default, and the plain host its known_hosts name
	config, creds, _ = Check(KindSFTP, Config{Address: "backup-node:22", User: "backup", Path: "restic", HostKey: hostKey}, Credentials{PrivateKey: privateKey})
	repo, _ = Locate(KindSFTP, config, creds)
	if config.Address != "backup-node" || !strings.HasPrefix(repo.KnownHosts, "backup-node ssh-ed25519 ") {
		t.Errorf("%+v %q", config, repo.KnownHosts)
	}

	// what ssh-keyscan prints: the server's name, a comment, every key of it
	other, _ := keys(t)
	bare := func(key string) string { return strings.Join(strings.Fields(key)[:2], " ") }
	scanned := "# backup-node:2222 SSH-2.0-OpenSSH_9.6\n[backup-node]:2222 " + hostKey + "\n\n[backup-node]:2222 " + other + " comment\n"
	config, creds, err = Check(KindSFTP, Config{Address: "backup-node:2222", User: "backup", Path: "/srv/restic", HostKey: scanned}, Credentials{PrivateKey: privateKey})
	if err != nil {
		t.Fatal(err)
	}
	repo, _ = Locate(KindSFTP, config, creds)
	if config.HostKey != bare(hostKey)+"\n"+bare(other) || repo.KnownHosts != "[backup-node]:2222 "+bare(hostKey)+"\n[backup-node]:2222 "+bare(other)+"\n" {
		t.Errorf("scanned keys: %q %q", config.HostKey, repo.KnownHosts)
	}
	if _, _, err := Check(KindSFTP, Config{Address: "backup-node", User: "backup", Path: "/r", HostKey: hostKey + "\nssh-ed25519 notakey"}, Credentials{PrivateKey: privateKey}); err == nil {
		t.Error("a line that is no key was taken")
	}

	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(mustKey(t), "", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		config Config
		key    string
		want   string
	}{
		"no host key":      {Config{Address: "backup-node", User: "backup", Path: "/r"}, privateKey, "host key"},
		"no private key":   {Config{Address: "backup-node", User: "backup", Path: "/r", HostKey: hostKey}, "nope", "private key"},
		"a passphrase":     {Config{Address: "backup-node", User: "backup", Path: "/r", HostKey: hostKey}, string(pem.EncodeToMemory(encrypted)), "passphrase"},
		"a bad user":       {Config{Address: "backup-node", User: "back up", Path: "/r", HostKey: hostKey}, privateKey, "user"},
		"a bad port":       {Config{Address: "backup-node:99999", User: "backup", Path: "/r", HostKey: hostKey}, privateKey, "address"},
		"a user in host":   {Config{Address: "root@backup-node", User: "backup", Path: "/r", HostKey: hostKey}, privateKey, "address"},
		"no path":          {Config{Address: "backup-node", User: "backup", HostKey: hostKey}, privateKey, "directory"},
		"a newline in dir": {Config{Address: "backup-node", User: "backup", Path: "/r\n-o x", HostKey: hostKey}, privateKey, "directory"},
	} {
		if _, _, err := Check(KindSFTP, test.config, Credentials{PrivateKey: test.key}); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func mustKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return private
}

func TestARestServerKeepsItsPasswordOutOfTheURL(t *testing.T) {
	config, creds, err := Check(KindREST, Config{URL: "https://backup-node:8000/homelab"}, Credentials{Username: "nas", Password: "pw-1234"})
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := Locate(KindREST, config, creds)
	if repo.URL != "rest:https://backup-node:8000/homelab/" || repo.Env["RESTIC_REST_USERNAME"] != "nas" || repo.Env["RESTIC_REST_PASSWORD"] != "pw-1234" {
		t.Errorf("%+v", repo)
	}
	// without auth, no credentials
	config, creds, _ = Check(KindREST, Config{URL: "http://10.0.0.9:8000/"}, Credentials{})
	if repo, _ := Locate(KindREST, config, creds); len(repo.Env) != 0 {
		t.Errorf("%+v", repo)
	}
	for name, test := range map[string]struct {
		url   string
		creds Credentials
	}{
		"credentials in the URL": {"https://nas:pw@backup-node:8000/", Credentials{}},
		"a user alone":           {"https://backup-node:8000/", Credentials{Username: "nas"}},
		"no scheme":              {"backup-node:8000", Credentials{}},
		"a query":                {"https://backup-node:8000/?x=1", Credentials{}},
	} {
		if _, _, err := Check(KindREST, Config{URL: test.url}, test.creds); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
}

func TestALocalTargetIsADirectoryOfItsOwn(t *testing.T) {
	config, creds, err := Check(KindLocal, Config{Path: "/srv/backups//restic/"}, Credentials{AccessKeyID: "dropped"})
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := Locate(KindLocal, config, creds)
	if config.Path != "/srv/backups/restic" || creds.AccessKeyID != "" || repo.URL != LocalMount || repo.Mount != "/srv/backups/restic" {
		t.Errorf("%+v %+v", config, repo)
	}
	for _, bad := range []string{"", "srv/backups", "/", "/etc/restic", "/var/lib/docker/volumes/x", "/proc", "/run/restic", "/srv/../etc/x"} {
		if _, _, err := Check(KindLocal, Config{Path: bad}, Credentials{}); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	if _, _, err := Check("ftp", Config{}, Credentials{}); err == nil {
		t.Error("an unknown kind was taken")
	}
}

func TestSecretsAreKeptOutOfWhatResticSaid(t *testing.T) {
	said := "Fatal: unable to open config file: Stat: The access key ID you provided does not exist (key-id-123, secret-xyz)"
	if got := Redact(said, "key-id-123", "secret-xyz", "", "ab"); strings.Contains(got, "key-id-123") || strings.Contains(got, "secret-xyz") || !strings.Contains(got, "(***, ***)") {
		t.Errorf("%q", got)
	}
	// a user or a region is no secret: restic's URLs and errors name them
	listed := Secrets("repo-password", map[string]string{"RESTIC_REST_USERNAME": "homelab", "RESTIC_REST_PASSWORD": "rest-password",
		"AWS_DEFAULT_REGION": "eu-central-003", "AWS_SECRET_ACCESS_KEY": "s3-secret"}, "")
	if len(listed) != 3 || slices.Contains(listed, "homelab") || slices.Contains(listed, "eu-central-003") || listed[0] != "repo-password" && listed[0] != "rest-password" {
		t.Errorf("secrets: %q", listed)
	}

	a, err := NewPassword()
	b, _ := NewPassword()
	if err != nil || len(a) != 64 || a == b {
		t.Errorf("%q %q %v", a, b, err)
	}
}

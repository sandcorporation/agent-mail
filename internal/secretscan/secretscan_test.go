package secretscan

import (
	"strings"
	"testing"
	"time"
)

// Fake keys are assembled at run time so the repository holds no literal
// key-shaped strings for push-protection scanners to flag.
const rnd = "Q7xR2mK9pL4vT8nB3wZ6yH1cF5dJ0gSa"

func fake(prefix string, n int) string {
	var b strings.Builder
	b.WriteString(prefix)
	for b.Len() < len(prefix)+n {
		b.WriteString(rnd)
	}
	return b.String()[:len(prefix)+n]
}

func upperDigits(n int) string {
	const s = "Z7Q2X9R4M8K3P6T1W5V0"
	return strings.Repeat(s, n/len(s)+1)[:n]
}

func TestScanFindsKeys(t *testing.T) {
	cases := []struct {
		name, key, kind string
	}{
		{"openai", fake("s"+"k-", 48), KindOpenAIKey},
		{"openai project", fake("s"+"k-"+"proj-", 64), KindOpenAIKey},
		{"anthropic", fake("s"+"k-"+"ant-"+"api03-", 90), KindAnthropicKey},
		{"github classic", fake("gh"+"p_", 36), KindGitHubToken},
		{"github oauth", fake("gh"+"o_", 36), KindGitHubToken},
		{"github fine-grained", fake("github"+"_pat_", 82), KindGitHubToken},
		{"slack bot", "xo" + "xb-" + "1234567890-9876543210-" + rnd[:24], KindSlackToken},
		{"slack user", "xo" + "xp-" + "1234567890-9876543210-" + rnd[:24], KindSlackToken},
		{"aws", "AK" + "IA" + upperDigits(16), KindAWSAccessKey},
		{"private key", "-----BEGIN " + "RSA PRIVATE" + " KEY-----", KindPrivateKey},
		{"openssh key", "-----BEGIN " + "OPENSSH PRIVATE" + " KEY-----", KindPrivateKey},
		{"private key plain", "-----BEGIN " + "PRIVATE" + " KEY-----", KindPrivateKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := "here it is: " + c.key + " (please rotate)"
			got := Scan(text)
			if len(got) != 1 {
				t.Fatalf("Scan found %v, want one %s", got, c.kind)
			}
			f := got[0]
			if f.Kind != c.kind {
				t.Fatalf("kind = %s, want %s", f.Kind, c.kind)
			}
			if text[f.Start:f.End] != c.key {
				t.Fatalf("span %d-%d does not cover the key", f.Start, f.End)
			}
		})
	}
}

func TestScanFindsSecretValues(t *testing.T) {
	val := "aZ3" + rnd[:21] + "x9Kq"
	texts := []string{
		"token: " + val,
		"TOKEN=" + val,
		"export AGENT_MAIL_TOKEN=" + val,
		`{"client_secret": "` + val + `"}`,
		"my password is " + val,
		"Password => '" + val + "'",
		"api_key=" + val,
		"토큰: " + val,
		"비밀번호는 " + val + " 입니다",
		"시크릿 " + val,
	}
	for _, text := range texts {
		got := Scan(text)
		if len(got) != 1 || got[0].Kind != KindSecretValue || text[got[0].Start:got[0].End] != val {
			t.Errorf("Scan(%q) = %v, want one secret_value over the value", text, got)
		}
	}
}

func TestScanIgnoresLookalikes(t *testing.T) {
	corpus := []string{
		// commits, ids and digests
		"fixed in 3c7ea94 and e958e6220b26d2658fc2fe0169451f8750572057",
		"token fix landed in e958e6220b26d2658fc2fe0169451f8750572057",
		"request id 123e4567-e89b-12d3-a456-426614174000",
		"secret id: 123e4567-e89b-12d3-a456-426614174000",
		"image sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"token digest sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"![chart](data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==)",
		"golang.org/x/crypto v0.40.0 h1:r4x+VvoG5Fm+eJcxMaY8CQM7Lb0l1lsmjGBQ6s8BfKM=",
		"golang.org/x/crypto v0.40.0/go.mod h1:Qr1vOvHuEp0rWRVqSYQ0mBZ5PZ+Yk1pAvGJXLx4tSkY=",
		"see https://example.test/search?q=token+rotation&page=2&sort=desc",
		// prose
		"The token expired yesterday, so please rotate the secret and update the password manager.",
		"Store the API token in a secret manager, never in source control.",
		"토큰이 만료되어 비밀번호를 바꾸고 시크릿을 다시 등록했습니다.",
		"set AGENT_MAIL_TOKEN in your environment before running the CLI",
		"AGENT_MAIL_TOKEN=",
		"GITHUB_TOKEN=${{ secrets.GITHUB_TOKEN }}",
		"password: internationalization",
		// placeholders
		"s" + "k-...",
		"export OPENAI_API_KEY=s" + "k-...",
		"token: <token>",
		"Authorization: Bearer <your-token-here>",
		"password=xxxxxxxxxxxxxxxxxxxx",
		"s" + "k-" + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		"gh" + "p_" + strings.Repeat("x", 36),
		"AK" + "IA" + "IOSFODNN7EXAMPLE",
		"-----BEGIN PUBLIC KEY-----",
		"-----BEGIN CERTIFICATE-----",
	}
	for _, text := range corpus {
		if got := Scan(text); len(got) != 0 {
			t.Errorf("Scan(%q) = %v, want none", text, got)
		}
	}
}

func TestFindingStringHasNoValue(t *testing.T) {
	key := fake("gh"+"p_", 36)
	got := Scan("x " + key)
	if len(got) != 1 {
		t.Fatalf("Scan = %v", got)
	}
	s := got[0].String()
	if strings.Contains(s, key[4:12]) || s != "github_token at 2-42" {
		t.Fatalf("String() = %q", s)
	}
}

func TestScanMultipleOrdered(t *testing.T) {
	a := fake("gh"+"p_", 36)
	b := "AK" + "IA" + upperDigits(16)
	got := Scan(a + " and then " + b)
	if len(got) != 2 || got[0].Kind != KindGitHubToken || got[1].Kind != KindAWSAccessKey || got[0].Start > got[1].Start {
		t.Fatalf("Scan = %v", got)
	}
}

func TestScanLargeBodyIsFast(t *testing.T) {
	line := "The token expired, see commit e958e6220b26d2658fc2fe0169451f8750572057 and 토큰 문서. "
	var b strings.Builder
	for b.Len() < 1<<20 {
		b.WriteString(line)
	}
	key := fake("gh"+"p_", 36)
	b.WriteString(key)
	body := b.String()
	began := time.Now()
	got := Scan(body)
	took := time.Since(began)
	if len(got) != 1 || got[0].Kind != KindGitHubToken {
		t.Fatalf("Scan on 1 MB = %v", got)
	}
	if took > 2*time.Second {
		t.Fatalf("Scan on 1 MB took %v", took)
	}
	t.Logf("1 MB scanned in %v", took)
}

func FuzzScan(f *testing.F) {
	f.Add("token: " + fake("", 30))
	f.Add(fake("s"+"k-"+"ant-", 40))
	f.Add("비밀번호는 " + fake("", 20))
	f.Add("-----BEGIN " + "PRIVATE" + " KEY-----")
	f.Add("\xff\xfe token=\x00")
	f.Fuzz(func(t *testing.T, s string) {
		prev := 0
		for _, fd := range Scan(s) {
			if fd.Start < prev || fd.End <= fd.Start || fd.End > len(s) {
				t.Fatalf("bad finding %v for len %d (prev end %d)", fd, len(s), prev)
			}
			prev = fd.End
		}
	})
}

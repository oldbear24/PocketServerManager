package stacks

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const goodFile = `services:
  web:
    image: nginx:1
    ports:
      - "8080:80"
    environment:
      TOKEN: ${TOKEN:?a token is needed}
    depends_on:
      - db
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: ${DB_PASS}
    volumes:
      - data:/var/lib/postgresql/data
volumes:
  data:
`

func validate(compose, env string, secrets ...string) Result {
	return Validate(context.Background(), "shop", compose, env, secrets)
}

func TestAValidFile(t *testing.T) {
	result := validate(goodFile, "TOKEN=abc\n", "DB_PASS")

	if !result.Valid || len(result.Errors) != 0 {
		t.Fatalf("%+v", result)
	}
	if strings.Join(result.Services, ",") != "db,web" {
		t.Errorf("services: %v", result.Services)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("warnings: %+v", result.Warnings)
	}
}

func TestProblemsPointAtTheLine(t *testing.T) {
	cases := []struct {
		name    string
		compose string
		line    int
		message string
	}{
		{
			name:    "a syntax error",
			compose: "services:\n  web:\n    image: nginx\n   ports: x\n",
			line:    4,
			message: "did not find expected key",
		},
		{
			name:    "an unfinished list",
			compose: "services:\n  web:\n    image: nginx\n    ports: [1, 2\n    x: y\n",
			line:    4,
			message: "expected",
		},
		{
			name:    "an unfinished string",
			compose: "services:\n  web:\n    image: 'nginx\n    x: y\n",
			line:    3,
			message: "end of stream",
		},
		{
			name:    "a tab",
			compose: "services:\n  web:\n\timage: nginx\n",
			line:    3,
			message: "",
		},
		{
			name:    "an unknown key in a service",
			compose: "services:\n  web:\n    image: nginx:1\n    imagee: typo\n",
			line:    4,
			message: "imagee",
		},
		{
			name:    "a wrong type",
			compose: "services:\n  web:\n    image: nginx:1\n    ports: 80\n",
			line:    4,
			message: "ports",
		},
		{
			name:    "a dependency that does not exist",
			compose: "services:\n  web:\n    image: nginx:1\n    depends_on:\n      - nobody\n",
			line:    2,
			message: "nobody",
		},
		{
			name:    "a volume that is not declared",
			compose: "services:\n  web:\n    image: nginx:1\n    volumes:\n      - nowhere:/data\n",
			line:    2,
			message: "nowhere",
		},
		{
			name:    "a required variable",
			compose: "services:\n  web:\n    image: nginx:1\n    environment:\n      A: ${MISSING:?tell me}\n",
			line:    0,
			message: "MISSING",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := validate(c.compose, "")

			if result.Valid {
				t.Fatalf("this must not be valid: %+v", result)
			}
			if len(result.Errors) != 1 {
				t.Fatalf("errors: %+v", result.Errors)
			}

			problem := result.Errors[0]
			if problem.Line != c.line {
				t.Errorf("line %d, want %d: %s", problem.Line, c.line, problem.Message)
			}
			if !strings.Contains(problem.Message, c.message) {
				t.Errorf("message: %s", problem.Message)
			}
			if problem.File != FileCompose {
				t.Errorf("file: %s", problem.File)
			}
		})
	}
}

func TestAnEmptyFileIsNotValid(t *testing.T) {
	result := validate("  \n", "")
	if result.Valid || len(result.Errors) != 1 || result.Errors[0].Line != 1 {
		t.Errorf("%+v", result)
	}
}

func TestAnInvalidEnvFile(t *testing.T) {
	result := validate(goodFile, "A=1\nB='not closed\n")
	if result.Valid || len(result.Errors) != 1 {
		t.Fatalf("%+v", result)
	}
	if result.Errors[0].File != FileEnv {
		t.Errorf("file: %+v", result.Errors[0])
	}
}

func TestReferencesToPanelSecretsAreFineInTheEnvFile(t *testing.T) {
	result := validate(goodFile, "TOKEN=${secret:db/shop}\nDB_PASS=x\n")
	if !result.Valid {
		t.Errorf("%+v", result)
	}
}

func TestRelativeBindMountsGetAWarning(t *testing.T) {
	result := validate("services:\n  web:\n    image: nginx:1\n    volumes:\n      - ./data:/data\n      - /srv/ok:/ok\n", "")

	if !result.Valid {
		t.Fatalf("%+v", result)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Line != 5 || !strings.Contains(result.Warnings[0].Message, "./data") {
		t.Errorf("warnings: %+v", result.Warnings)
	}
}

func TestResolveEnvMovesReferencesOutOfTheFile(t *testing.T) {
	plain, secrets, err := ResolveEnv("TZ=Europe/Prague\nDB_PASSWORD=${secret:db/shop}\nexport URL=postgres://shop:${secret:db/shop}@db/shop\n# a comment\n", func(name string) (string, error) {
		if name != "db/shop" {
			return "", ErrUnknownSecret
		}
		return "hunter2", nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(plain, "secret:") || strings.Contains(plain, "hunter2") {
		t.Errorf("the plain file holds a secret: %q", plain)
	}
	if !strings.Contains(plain, "TZ=Europe/Prague") || !strings.Contains(plain, "# a comment") {
		t.Errorf("the plain file lost lines: %q", plain)
	}
	if secrets["DB_PASSWORD"] != "hunter2" || secrets["URL"] != "postgres://shop:hunter2@db/shop" {
		t.Errorf("secrets: %v", secrets)
	}

	_, _, err = ResolveEnv("A=${secret:gone}\n", func(string) (string, error) { return "", ErrUnknownSecret })
	if !errors.Is(err, ErrUnknownSecret) || !strings.Contains(err.Error(), "A") {
		t.Errorf("an unknown secret: %v", err)
	}
}

func TestEnvKeys(t *testing.T) {
	keys := EnvKeys("B=2\n# comment\nA=1\nC=${secret:x}\n")
	if strings.Join(keys, ",") != "A,B,C" {
		t.Errorf("keys: %v", keys)
	}
}

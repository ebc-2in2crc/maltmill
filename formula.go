package maltmill

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/google/go-github/github"
	"github.com/pkg/errors"
)

type formula struct {
	fname string

	content                    string
	urlTmpl                    string
	isURLTmpl                  bool
	name, version, url, sha256 string
	owner, repo                string
}

var (
	nameReg = regexp.MustCompile(`(?m)^\s+name\s*=\s*['"](.*)["']`)
	verReg  = regexp.MustCompile(`(?m)(^\s+version\s*['"])(.*)(["'])`)
	urlReg  = regexp.MustCompile(`(?m)(^\s+url\s*['"])(.*)(["'])`)
	shaReg  = regexp.MustCompile(`(?m)(\s+sha256\s*['"])(.*)(["'])`)

	parseURLReg = regexp.MustCompile(`^https://[^/]*github.com/([^/]+)/([^/]+)`)
)

func newFormula(f string) (*formula, error) {
	b, err := ioutil.ReadFile(f)
	if err != nil {
		return nil, err
	}
	fo := &formula{fname: f}
	fo.content = string(b)

	if m := nameReg.FindStringSubmatch(fo.content); len(m) > 1 {
		fo.name = m[1]
	}
	m := verReg.FindStringSubmatch(fo.content)
	if len(m) < 4 {
		return nil, errors.New("no version detected")
	}
	fo.version = m[2]

	m = shaReg.FindStringSubmatch(fo.content)
	if len(m) < 4 {
		return nil, errors.New("no sha256 detected")
	}
	fo.sha256 = m[2]

	info := map[string]string{
		"name":    fo.name,
		"version": fo.version,
	}

	m = urlReg.FindStringSubmatch(fo.content)
	if len(m) < 4 {
		return nil, errors.New("no url detected")
	}
	fo.urlTmpl = m[2]
	fo.isURLTmpl = strings.Contains(fo.urlTmpl, "#{version}")

	if fo.isURLTmpl {
		fo.url, err = expandStr(fo.urlTmpl, info)
		if err != nil {
			return nil, err
		}
	} else {
		fo.url = fo.urlTmpl
	}

	m = parseURLReg.FindStringSubmatch(fo.url)
	if len(m) < 3 {
		return nil, errors.Errorf("invalid url format: %s", fo.urlTmpl)
	}
	fo.owner = m[1]
	fo.repo = m[2]

	return fo, nil
}

func expandStr(str string, m map[string]string) (string, error) {
	for k, v := range m {
		reg, err := regexp.Compile(`#{` + k + `}`)
		if err != nil {
			return "", err
		}
		str = reg.ReplaceAllString(str, v)
	}
	return str, nil
}

func (fo *formula) update(ghcli *github.Client) (updated bool, err error) {
	origVer, err := semver.NewVersion(fo.version)
	if err != nil {
		return false, errors.Wrap(err, "invalid original version")
	}

	rele, resp, err := ghcli.Repositories.GetLatestRelease(context.Background(), fo.owner, fo.repo)
	if err != nil {
		return false, errors.Wrapf(err, "update formula failed: %s", fo.fname)
	}
	resp.Body.Close()

	newVer, err := semver.NewVersion(rele.GetTagName())
	if err != nil {
		return false, errors.Wrapf(err, "invalid original version. formula: %s", fo.fname)
	}
	if !origVer.LessThan(newVer) {
		return false, nil
	}

	newVerStr := fmt.Sprintf("%d.%d.%d", newVer.Major(), newVer.Minor(), newVer.Patch())
	var newURL string
	if fo.isURLTmpl {
		newURL, err = expandStr(fo.urlTmpl, map[string]string{
			"name":    fo.name,
			"version": newVerStr,
		})
		if err != nil {
			return false, errors.Wrapf(err, "faild to upload formula: %s", fo.fname)
		}
	} else {
		newURL, err = func() (string, error) {
			ext := path.Ext(fo.url)
			for _, asset := range rele.Assets {
				u := asset.GetBrowserDownloadURL()
				fname := path.Base(u)
				// edit distance is better?
				if strings.Contains(fname, "amd64") &&
					strings.Contains(fname, "darwin") &&
					strings.HasSuffix(fname, ext) {
					return u, nil
				}
			}
			return "", errors.New("no assets found from latest release")
		}()
		if err != nil {
			return false, err
		}
	}

	newSHA256, err := getSHA256FromURL(newURL)
	if err != nil {
		return false, errors.Wrapf(err, "faild to upload formula: %s", fo.fname)
	}
	fo.version = newVerStr
	fo.url = newURL
	fo.sha256 = newSHA256
	fo.updateContent()

	return true, nil
}

// update version and sha256
func (fo *formula) updateContent() {
	fo.content = replaceOne(verReg, fo.content, fmt.Sprintf(`${1}%s${3}`, fo.version))
	fo.content = replaceOne(shaReg, fo.content, fmt.Sprintf(`${1}%s${3}`, fo.sha256))
	if !fo.isURLTmpl {
		fo.content = replaceOne(urlReg, fo.content, fmt.Sprintf(`${1}%s${3}`, fo.url))
	}
}

func replaceOne(reg *regexp.Regexp, str, replace string) string {
	replaced := false
	return reg.ReplaceAllStringFunc(str, func(match string) string {
		if replaced {
			return match
		}
		replaced = true
		return reg.ReplaceAllString(match, replace)
	})
}

func getSHA256FromURL(u string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("maltmill/%s", version))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", errors.Wrapf(err, "getSHA256 failed while request to url: %s", u)
	}
	defer resp.Body.Close()

	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", errors.Wrapf(err, "getSHA256 failed while reading response. url: %s", u)
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Package registry checks that packages a model wants to install are real
// and reasonably popular, as a guard against hallucinated or typo-squatted names.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	MinWeekly = 1000    // for registries that report recent downloads
	MinTotal  = 100_000 // for registries that only report all-time totals
)

type Finding struct {
	Ecosystem string
	Package   string
	Exists    bool
	Weekly    int64 // -1 when the registry doesn't report it
	Total     int64 // -1 when the registry doesn't report it
	Err       error
}

func (f Finding) OK() bool {
	if f.Err != nil || !f.Exists {
		return false
	}
	if f.Weekly >= 0 {
		return f.Weekly >= MinWeekly
	}
	return f.Total >= MinTotal
}

var registryName = map[string]string{
	"npm": "npm", "pypi": "PyPI", "crates": "crates.io", "packagist": "Packagist",
	"rubygems": "RubyGems", "nuget": "NuGet", "pub": "pub.dev",
}

func (f Finding) Describe() string {
	where := registryName[f.Ecosystem]
	switch {
	case f.Err != nil:
		return fmt.Sprintf("Couldn't verify %s on %s (%v), so this needs your approval.", f.Package, where, f.Err)
	case !f.Exists:
		return fmt.Sprintf("%s was not found on %s. The model may have invented it, or it may be a typo of a real package.", f.Package, where)
	case f.Weekly >= 0 && f.Weekly < MinWeekly:
		return fmt.Sprintf("%s has only %s weekly downloads on %s. Check it's the package you expect.", f.Package, commas(f.Weekly), where)
	case f.Weekly >= 0:
		return fmt.Sprintf("%s: %s weekly downloads on %s", f.Package, commas(f.Weekly), where)
	case f.Total >= 0 && f.Total < MinTotal:
		return fmt.Sprintf("%s has only %s downloads in total on %s. Check it's the package you expect.", f.Package, commas(f.Total), where)
	case f.Total >= 0:
		return fmt.Sprintf("%s: %s total downloads on %s", f.Package, commas(f.Total), where)
	}
	return fmt.Sprintf("%s exists on %s but its download count is unavailable, so this needs your approval.", f.Package, where)
}

var (
	client          = &http.Client{Timeout: 6 * time.Second}
	npmNameRe       = regexp.MustCompile(`(?i)^(@[a-z0-9][a-z0-9._~-]*/)?[a-z0-9][a-z0-9._~-]*$`)
	pypiNameRe      = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	crateNameRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	packagistNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*/[a-z0-9][a-z0-9_.-]*$`)
	gemNameRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	nugetNameRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	pubNameRe       = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	pypiSepRe       = regexp.MustCompile(`[-_.]+`)
	errBadName      = errors.New("not a valid package name")
)

func Check(ctx context.Context, ecosystem, name string) Finding {
	switch ecosystem {
	case "npm":
		return npm(ctx, name)
	case "pypi":
		return pypi(ctx, name)
	case "crates":
		return crates(ctx, name)
	case "packagist":
		return packagist(ctx, name)
	case "rubygems":
		return rubygems(ctx, name)
	case "nuget":
		return nuget(ctx, name)
	case "pub":
		return pub(ctx, name)
	}
	return Finding{Ecosystem: ecosystem, Package: name, Weekly: -1, Total: -1, Err: errors.New("unsupported ecosystem")}
}

func newFinding(eco, name string) Finding {
	return Finding{Ecosystem: eco, Package: name, Weekly: -1, Total: -1}
}

// record applies a lookup's outcome: found on 200, absent on 404, error otherwise.
func record(f *Finding, status int, err error) bool {
	switch {
	case err != nil:
		f.Err = err
	case status == http.StatusNotFound:
	case status == http.StatusOK:
		f.Exists = true
		return true
	default:
		f.Err = fmt.Errorf("HTTP %d", status)
	}
	return false
}

func npm(ctx context.Context, name string) Finding {
	f := newFinding("npm", name)
	if !npmNameRe.MatchString(name) {
		f.Err = errBadName
		return f
	}
	var out struct {
		Downloads int64 `json:"downloads"`
	}
	status, err := get(ctx, "https://api.npmjs.org/downloads/point/last-week/"+name, &out)
	if record(&f, status, err) {
		f.Weekly = out.Downloads
	}
	return f
}

func pypi(ctx context.Context, name string) Finding {
	f := newFinding("pypi", name)
	if !pypiNameRe.MatchString(name) {
		f.Err = errBadName
		return f
	}
	n := strings.ToLower(pypiSepRe.ReplaceAllString(name, "-"))
	status, err := get(ctx, "https://pypi.org/pypi/"+n+"/json", nil)
	if !record(&f, status, err) {
		return f
	}
	var st struct {
		Data struct {
			LastWeek int64 `json:"last_week"`
		} `json:"data"`
	}
	if s, err := get(ctx, "https://pypistats.org/api/packages/"+n+"/recent", &st); err == nil && s == http.StatusOK {
		f.Weekly = st.Data.LastWeek
	}
	return f
}

func crates(ctx context.Context, name string) Finding {
	f := newFinding("crates", name)
	if !crateNameRe.MatchString(name) {
		f.Err = errBadName
		return f
	}
	var out struct {
		Crate struct {
			RecentDownloads int64 `json:"recent_downloads"` // last 90 days
		} `json:"crate"`
	}
	status, err := get(ctx, "https://crates.io/api/v1/crates/"+name, &out)
	if record(&f, status, err) {
		f.Weekly = out.Crate.RecentDownloads / 13
	}
	return f
}

func packagist(ctx context.Context, name string) Finding {
	f := newFinding("packagist", name)
	if !packagistNameRe.MatchString(name) {
		f.Err = errBadName
		return f
	}
	var out struct {
		Package struct {
			Downloads struct {
				Monthly int64 `json:"monthly"`
			} `json:"downloads"`
		} `json:"package"`
	}
	status, err := get(ctx, "https://packagist.org/packages/"+name+".json", &out)
	if record(&f, status, err) {
		f.Weekly = out.Package.Downloads.Monthly * 7 / 30
	}
	return f
}

func rubygems(ctx context.Context, name string) Finding {
	f := newFinding("rubygems", name)
	if !gemNameRe.MatchString(name) {
		f.Err = errBadName
		return f
	}
	var out struct {
		Downloads int64 `json:"downloads"` // all-time
	}
	status, err := get(ctx, "https://rubygems.org/api/v1/gems/"+name+".json", &out)
	if record(&f, status, err) {
		f.Total = out.Downloads
	}
	return f
}

func nuget(ctx context.Context, name string) Finding {
	f := newFinding("nuget", name)
	if !nugetNameRe.MatchString(name) {
		f.Err = errBadName
		return f
	}
	var out struct {
		Data []struct {
			ID             string `json:"id"`
			TotalDownloads int64  `json:"totalDownloads"`
		} `json:"data"`
	}
	q := "https://azuresearch-usnc.nuget.org/query?prerelease=true&take=1&q=" + url.QueryEscape("packageid:"+name)
	status, err := get(ctx, q, &out)
	if err != nil || status != http.StatusOK {
		record(&f, status, err)
		f.Exists = false
		return f
	}
	for _, d := range out.Data {
		if strings.EqualFold(d.ID, name) {
			f.Exists, f.Total = true, d.TotalDownloads
		}
	}
	return f
}

func pub(ctx context.Context, name string) Finding {
	f := newFinding("pub", name)
	if !pubNameRe.MatchString(name) {
		f.Err = errBadName
		return f
	}
	var out struct {
		DownloadCount30Days *int64 `json:"downloadCount30Days"` // null for brand-new packages
	}
	status, err := get(ctx, "https://pub.dev/api/packages/"+name+"/score", &out)
	if record(&f, status, err) && out.DownloadCount30Days != nil {
		f.Weekly = *out.DownloadCount30Days * 7 / 30
	}
	return f
}

func get(ctx context.Context, u string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	// crates.io rejects requests without a User-Agent.
	req.Header.Set("User-Agent", "lazyfinger-cli (+https://github.com/himanshupphp/lazyfinger)")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

package gps

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// A remoteRepo represents a potential remote repository resource.
//
// RemoteRepos are based purely on lexical analysis; successfully constructing
// one is not a guarantee that the resource it identifies actually exists or is
// accessible.
type remoteRepo struct {
	Base     string
	RelPkg   string
	CloneURL *url.URL
	Schemes  []string
	VCS      []string
}

type futureString func() (string, error)
type futureSource func() (source, error)
type deferredFutureSource func(string, ProjectAnalyzer) futureSource

var (
	gitSchemes = []string{"https", "ssh", "git", "http"}
	bzrSchemes = []string{"https", "bzr+ssh", "bzr", "http"}
	hgSchemes  = []string{"https", "ssh", "http"}
	svnSchemes = []string{"https", "http", "svn", "svn+ssh"}
)

func validateVCSScheme(scheme, typ string) bool {
	var schemes []string
	switch typ {
	case "git":
		schemes = gitSchemes
	case "bzr":
		schemes = bzrSchemes
	case "hg":
		schemes = hgSchemes
	case "svn":
		schemes = svnSchemes
	default:
		panic(fmt.Sprint("unsupported vcs type", scheme))
	}

	for _, valid := range schemes {
		if scheme == valid {
			return true
		}
	}
	return false
}

// Regexes for the different known import path flavors
var (
	// This regex allowed some usernames that github currently disallows. They
	// may have allowed them in the past; keeping it in case we need to revert.
	//ghRegex      = regexp.MustCompile(`^(?P<root>github\.com/([A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+))(/[A-Za-z0-9_.\-]+)*$`)
	ghRegex      = regexp.MustCompile(`^(?P<root>github\.com/([A-Za-z0-9][-A-Za-z0-9]*[A-Za-z0-9]/[A-Za-z0-9_.\-]+))((?:/[A-Za-z0-9_.\-]+)*)$`)
	gpinNewRegex = regexp.MustCompile(`^(?P<root>gopkg\.in/(?:([a-zA-Z0-9][-a-zA-Z0-9]+)/)?([a-zA-Z][-.a-zA-Z0-9]*)\.((?:v0|v[1-9][0-9]*)(?:\.0|\.[1-9][0-9]*){0,2}(-unstable)?)(?:\.git)?)((?:/[a-zA-Z0-9][-.a-zA-Z0-9]*)*)$`)
	//gpinOldRegex = regexp.MustCompile(`^(?P<root>gopkg\.in/(?:([a-z0-9][-a-z0-9]+)/)?((?:v0|v[1-9][0-9]*)(?:\.0|\.[1-9][0-9]*){0,2}(-unstable)?)/([a-zA-Z][-a-zA-Z0-9]*)(?:\.git)?)((?:/[a-zA-Z][-a-zA-Z0-9]*)*)$`)
	bbRegex = regexp.MustCompile(`^(?P<root>bitbucket\.org/(?P<bitname>[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+))((?:/[A-Za-z0-9_.\-]+)*)$`)
	//lpRegex = regexp.MustCompile(`^(?P<root>launchpad\.net/([A-Za-z0-9-._]+)(/[A-Za-z0-9-._]+)?)(/.+)?`)
	lpRegex = regexp.MustCompile(`^(?P<root>launchpad\.net/([A-Za-z0-9-._]+))((?:/[A-Za-z0-9_.\-]+)*)?`)
	//glpRegex = regexp.MustCompile(`^(?P<root>git\.launchpad\.net/([A-Za-z0-9_.\-]+)|~[A-Za-z0-9_.\-]+/(\+git|[A-Za-z0-9_.\-]+)/[A-Za-z0-9_.\-]+)$`)
	glpRegex = regexp.MustCompile(`^(?P<root>git\.launchpad\.net/([A-Za-z0-9_.\-]+))((?:/[A-Za-z0-9_.\-]+)*)$`)
	//gcRegex      = regexp.MustCompile(`^(?P<root>code\.google\.com/[pr]/(?P<project>[a-z0-9\-]+)(\.(?P<subrepo>[a-z0-9\-]+))?)(/[A-Za-z0-9_.\-]+)*$`)
	jazzRegex         = regexp.MustCompile(`^(?P<root>hub\.jazz\.net/(git/[a-z0-9]+/[A-Za-z0-9_.\-]+))((?:/[A-Za-z0-9_.\-]+)*)$`)
	apacheRegex       = regexp.MustCompile(`^(?P<root>git\.apache\.org/([a-z0-9_.\-]+\.git))((?:/[A-Za-z0-9_.\-]+)*)$`)
	vcsExtensionRegex = regexp.MustCompile(`^(?P<root>(?P<repo>([a-z0-9.\-]+\.)+[a-z0-9.\-]+(:[0-9]+)?/[A-Za-z0-9_.\-/~]*?)\.(?P<vcs>bzr|git|hg|svn))((?:/[A-Za-z0-9_.\-]+)*)$`)
)

// Other helper regexes
var (
	scpSyntaxRe = regexp.MustCompile(`^([a-zA-Z0-9_]+)@([a-zA-Z0-9._-]+):(.*)$`)
	pathvld     = regexp.MustCompile(`^([A-Za-z0-9-]+)(\.[A-Za-z0-9-]+)+(/[A-Za-z0-9-_.~]+)*$`)
)

func simpleStringFuture(s string) futureString {
	return func() (string, error) {
		return s, nil
	}
}

func sourceFutureFactory(mb maybeSource) func(string, ProjectAnalyzer) futureSource {
	return func(cachedir string, an ProjectAnalyzer) futureSource {
		var src source
		var err error

		c := make(chan struct{}, 1)
		go func() {
			defer close(c)
			src, err = mb.try(cachedir, an)
		}()

		return func() (source, error) {
			<-c
			return src, err
		}
	}
}

type matcher interface {
	deduceRoot(string) (futureString, error)
	deduceSource(string, *url.URL) (func(string, ProjectAnalyzer) futureSource, error)
}

type githubMatcher struct {
	regexp *regexp.Regexp
}

func (m githubMatcher) deduceRoot(path string) (futureString, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on github.com", path)
	}

	return simpleStringFuture("github.com/" + v[2]), nil
}

func (m githubMatcher) deduceSource(path string, u *url.URL) (func(string, ProjectAnalyzer) futureSource, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on github.com", path)
	}

	u.Path = v[2]
	if u.Scheme != "" {
		if !validateVCSScheme(u.Scheme, "git") {
			return nil, fmt.Errorf("%s is not a valid scheme for accessing a git repository", u.Scheme)
		}
		return sourceFutureFactory(maybeGitSource{url: u}), nil
	}

	mb := make(maybeSources, len(gitSchemes))
	for k, scheme := range gitSchemes {
		u2 := *u
		u2.Scheme = scheme
		mb[k] = maybeGitSource{url: &u2}
	}

	return sourceFutureFactory(mb), nil
}

type bitbucketMatcher struct {
	regexp *regexp.Regexp
}

func (m bitbucketMatcher) deduceRoot(path string) (futureString, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on bitbucket.org", path)
	}

	return simpleStringFuture("bitbucket.org/" + v[2]), nil
}

func (m bitbucketMatcher) deduceSource(path string, u *url.URL) (func(string, ProjectAnalyzer) futureSource, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on bitbucket.org", path)
	}
	u.Path = v[2]

	// This isn't definitive, but it'll probably catch most
	isgit := strings.HasSuffix(u.Path, ".git") || u.User.Username() == "git"
	ishg := strings.HasSuffix(u.Path, ".hg") || u.User.Username() == "hg"

	if u.Scheme != "" {
		validgit, validhg := validateVCSScheme(u.Scheme, "git"), validateVCSScheme(u.Scheme, "hg")
		if isgit {
			if !validgit {
				return nil, fmt.Errorf("%s is not a valid scheme for accessing a git repository", u.Scheme)
			}
			return sourceFutureFactory(maybeGitSource{url: u}), nil
		} else if ishg {
			if !validhg {
				return nil, fmt.Errorf("%s is not a valid scheme for accessing an hg repository", u.Scheme)
			}
			return sourceFutureFactory(maybeHgSource{url: u}), nil
		} else if !validgit && !validhg {
			return nil, fmt.Errorf("%s is not a valid scheme for accessing either a git or hg repository", u.Scheme)
		}

		// No other choice, make an option for both git and hg
		return sourceFutureFactory(maybeSources{
			// Git first, because it's a) faster and b) git
			maybeGitSource{url: u},
			maybeHgSource{url: u},
		}), nil
	}

	mb := make(maybeSources, 0)
	if !ishg {
		for _, scheme := range gitSchemes {
			u2 := *u
			u2.Scheme = scheme
			mb = append(mb, maybeGitSource{url: &u2})
		}
	}

	if !isgit {
		for _, scheme := range hgSchemes {
			u2 := *u
			u2.Scheme = scheme
			mb = append(mb, maybeHgSource{url: &u2})
		}
	}

	return sourceFutureFactory(mb), nil
}

type gopkginMatcher struct {
	regexp *regexp.Regexp
}

func (m gopkginMatcher) deduceRoot(path string) (futureString, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on gopkg.in", path)
	}

	return simpleStringFuture("gopkg.in/" + v[2]), nil
}

func (m gopkginMatcher) deduceSource(path string, u *url.URL) (func(string, ProjectAnalyzer) futureSource, error) {

	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on gopkg.in", path)
	}

	// Duplicate some logic from the gopkg.in server in order to validate
	// the import path string without having to hit the server
	if strings.Contains(v[4], ".") {
		return nil, fmt.Errorf("%q is not a valid import path; gopkg.in only allows major versions (%q instead of %q)",
			path, v[4][:strings.Index(v[4], ".")], v[4])
	}

	// Putting a scheme on gopkg.in would be really weird, disallow it
	if u.Scheme != "" {
		return nil, fmt.Errorf("Specifying alternate schemes on gopkg.in imports is not permitted")
	}

	// gopkg.in is always backed by github
	u.Host = "github.com"
	// If the third position is empty, it's the shortened form that expands
	// to the go-pkg github user
	if v[2] == "" {
		u.Path = "go-pkg/" + v[3]
	} else {
		u.Path = v[2] + "/" + v[3]
	}

	mb := make(maybeSources, len(gitSchemes))
	for k, scheme := range gitSchemes {
		u2 := *u
		u2.Scheme = scheme
		mb[k] = maybeGitSource{url: &u2}
	}

	return sourceFutureFactory(mb), nil
}

type launchpadMatcher struct {
	regexp *regexp.Regexp
}

func (m launchpadMatcher) deduceRoot(path string) (futureString, error) {
	// TODO(sdboyer) lp handling is nasty - there's ambiguities which can only really
	// be resolved with a metadata request. See https://github.com/golang/go/issues/11436
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on launchpad.net", path)
	}

	return simpleStringFuture("launchpad.net/" + v[2]), nil
}

func (m launchpadMatcher) deduceSource(path string, u *url.URL) (func(string, ProjectAnalyzer) futureSource, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on launchpad.net", path)
	}

	u.Path = v[2]
	if u.Scheme != "" {
		if !validateVCSScheme(u.Scheme, "bzr") {
			return nil, fmt.Errorf("%s is not a valid scheme for accessing a bzr repository", u.Scheme)
		}
		return sourceFutureFactory(maybeBzrSource{url: u}), nil
	}

	mb := make(maybeSources, len(bzrSchemes))
	for k, scheme := range bzrSchemes {
		u2 := *u
		u2.Scheme = scheme
		mb[k] = maybeBzrSource{url: &u2}
	}

	return sourceFutureFactory(mb), nil
}

type launchpadGitMatcher struct {
	regexp *regexp.Regexp
}

func (m launchpadGitMatcher) deduceRoot(path string) (futureString, error) {
	// TODO(sdboyer) same ambiguity issues as with normal bzr lp
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on git.launchpad.net", path)
	}

	return simpleStringFuture("git.launchpad.net/" + v[2]), nil
}

func (m launchpadGitMatcher) deduceSource(path string, u *url.URL) (func(string, ProjectAnalyzer) futureSource, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on git.launchpad.net", path)
	}

	u.Path = v[2]
	if u.Scheme != "" {
		if !validateVCSScheme(u.Scheme, "git") {
			return nil, fmt.Errorf("%s is not a valid scheme for accessing a git repository", u.Scheme)
		}
		return sourceFutureFactory(maybeGitSource{url: u}), nil
	}

	mb := make(maybeSources, len(bzrSchemes))
	for k, scheme := range bzrSchemes {
		u2 := *u
		u2.Scheme = scheme
		mb[k] = maybeGitSource{url: &u2}
	}

	return sourceFutureFactory(mb), nil
}

type jazzMatcher struct {
	regexp *regexp.Regexp
}

func (m jazzMatcher) deduceRoot(path string) (futureString, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on hub.jazz.net", path)
	}

	return simpleStringFuture("hub.jazz.net/" + v[2]), nil
}

func (m jazzMatcher) deduceSource(path string, u *url.URL) (func(string, ProjectAnalyzer) futureSource, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on hub.jazz.net", path)
	}

	u.Path = v[2]
	if u.Scheme != "" {
		if !validateVCSScheme(u.Scheme, "git") {
			return nil, fmt.Errorf("%s is not a valid scheme for accessing a git repository", u.Scheme)
		}
		return sourceFutureFactory(maybeGitSource{url: u}), nil
	}

	mb := make(maybeSources, len(gitSchemes))
	for k, scheme := range gitSchemes {
		u2 := *u
		u2.Scheme = scheme
		mb[k] = maybeGitSource{url: &u2}
	}

	return sourceFutureFactory(mb), nil
}

type apacheMatcher struct {
	regexp *regexp.Regexp
}

func (m apacheMatcher) deduceRoot(path string) (futureString, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on git.apache.org", path)
	}

	return simpleStringFuture("git.apache.org/" + v[2]), nil
}

func (m apacheMatcher) deduceSource(path string, u *url.URL) (func(string, ProjectAnalyzer) futureSource, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s is not a valid path for a source on git.apache.org", path)
	}

	u.Path = v[2]
	if u.Scheme != "" {
		if !validateVCSScheme(u.Scheme, "git") {
			return nil, fmt.Errorf("%s is not a valid scheme for accessing a git repository", u.Scheme)
		}
		return sourceFutureFactory(maybeGitSource{url: u}), nil
	}

	mb := make(maybeSources, len(gitSchemes))
	for k, scheme := range gitSchemes {
		u2 := *u
		u2.Scheme = scheme
		mb[k] = maybeGitSource{url: &u2}
	}

	return sourceFutureFactory(mb), nil
}

type vcsExtensionMatcher struct {
	regexp *regexp.Regexp
}

func (m vcsExtensionMatcher) deduceRoot(path string) (futureString, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s contains no vcs extension hints for matching", path)
	}

	return simpleStringFuture(v[1]), nil
}

func (m vcsExtensionMatcher) deduceSource(path string, u *url.URL) (func(string, ProjectAnalyzer) futureSource, error) {
	v := m.regexp.FindStringSubmatch(path)
	if v == nil {
		return nil, fmt.Errorf("%s contains no vcs extension hints for matching", path)
	}

	switch v[5] {
	case "git", "hg", "bzr":
		x := strings.SplitN(v[1], "/", 2)
		// TODO(sdboyer) is this actually correct for bzr?
		u.Host = x[0]
		u.Path = x[1]

		if u.Scheme != "" {
			if !validateVCSScheme(u.Scheme, v[5]) {
				return nil, fmt.Errorf("%s is not a valid scheme for accessing %s repositories (path %s)", u.Scheme, v[5], path)
			}

			switch v[5] {
			case "git":
				return sourceFutureFactory(maybeGitSource{url: u}), nil
			case "bzr":
				return sourceFutureFactory(maybeBzrSource{url: u}), nil
			case "hg":
				return sourceFutureFactory(maybeHgSource{url: u}), nil
			}
		}

		var schemes []string
		var mb maybeSources
		var f func(k int, u *url.URL)
		switch v[5] {
		case "git":
			schemes = gitSchemes
			f = func(k int, u *url.URL) {
				mb[k] = maybeGitSource{url: u}
			}
		case "bzr":
			schemes = bzrSchemes
			f = func(k int, u *url.URL) {
				mb[k] = maybeBzrSource{url: u}
			}
		case "hg":
			schemes = hgSchemes
			f = func(k int, u *url.URL) {
				mb[k] = maybeHgSource{url: u}
			}
		}
		mb = make(maybeSources, len(schemes))

		for k, scheme := range gitSchemes {
			u2 := *u
			u2.Scheme = scheme
			f(k, &u2)
		}

		return sourceFutureFactory(mb), nil
	default:
		return nil, fmt.Errorf("unknown repository type: %q", v[5])
	}
}

// deduceFromPath takes an import path and converts it into a valid source root.
//
// The result is wrapped in a future, as some import path patterns may require
// network activity to correctly determine them via the parsing of "go get" HTTP
// meta tags.
func (sm *SourceMgr) deduceFromPath(path string) (root futureString, src deferredFutureSource, err error) {
	u, err := normalizeURI(path)
	if err != nil {
		return nil, nil, err
	}

	// First, try the root path-based matches
	if _, mtchi, has := sm.rootxt.LongestPrefix(path); has {
		mtch := mtchi.(matcher)
		root, err = mtch.deduceRoot(path)
		if err != nil {
			return nil, nil, err
		}
		src, err = mtch.deduceSource(path, u)
		if err != nil {
			return nil, nil, err
		}

		return
	}

	// Next, try the vcs extension-based (infix) matcher
	exm := vcsExtensionMatcher{regexp: vcsExtensionRegex}
	if root, err = exm.deduceRoot(path); err == nil {
		src, err = exm.deduceSource(path, u)
		if err != nil {
			root, src = nil, nil
		}
		return
	}

	// No luck so far. maybe it's one of them vanity imports?
	// We have to get a little fancier for the metadata lookup by chaining the
	// source future onto the metadata future

	// Declare these out here so they're available for the source future
	var vcs string
	var ru *url.URL

	// Kick off the vanity metadata fetch
	var importroot string
	var futerr error
	c := make(chan struct{}, 1)
	go func() {
		defer close(c)
		var reporoot string
		importroot, vcs, reporoot, futerr = parseMetadata(path)
		if futerr != nil {
			futerr = fmt.Errorf("unable to deduce repository and source type for: %q", path)
			return
		}

		// If we got something back at all, then it supercedes the actual input for
		// the real URL to hit
		ru, futerr = url.Parse(reporoot)
		if futerr != nil {
			futerr = fmt.Errorf("server returned bad URL when searching for vanity import: %q", reporoot)
			importroot = ""
			return
		}
	}()

	// Set up the root func to catch the result
	root = func() (string, error) {
		<-c
		return importroot, futerr
	}

	src = func(cachedir string, an ProjectAnalyzer) futureSource {
		var src source
		var err error

		c := make(chan struct{}, 1)
		go func() {
			defer close(c)
			// make sure the metadata future is finished (without errors), thus
			// guaranteeing that ru and vcs will be populated
			_, err := root()
			if err != nil {
				return
			}

			var m maybeSource
			switch vcs {
			case "git":
				m = maybeGitSource{url: ru}
			case "bzr":
				m = maybeBzrSource{url: ru}
			case "hg":
				m = maybeHgSource{url: ru}
			}

			if m != nil {
				src, err = m.try(cachedir, an)
			} else {
				err = fmt.Errorf("unsupported vcs type %s", vcs)
			}
		}()

		return func() (source, error) {
			<-c
			return src, err
		}
	}

	return
}

func normalizeURI(path string) (u *url.URL, err error) {
	if m := scpSyntaxRe.FindStringSubmatch(path); m != nil {
		// Match SCP-like syntax and convert it to a URL.
		// Eg, "git@github.com:user/repo" becomes
		// "ssh://git@github.com/user/repo".
		u = &url.URL{
			Scheme: "ssh",
			User:   url.User(m[1]),
			Host:   m[2],
			Path:   "/" + m[3],
			// TODO(sdboyer) This is what stdlib sets; grok why better
			//RawPath: m[3],
		}
	} else {
		u, err = url.Parse(path)
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid URI", path)
		}
	}

	if u.Host != "" {
		path = u.Host + "/" + strings.TrimPrefix(u.Path, "/")
	} else {
		path = u.Path
	}

	if !pathvld.MatchString(path) {
		return nil, fmt.Errorf("%q is not a valid import path", path)
	}

	return
}

// deduceRemoteRepo takes a potential import path and returns a RemoteRepo
// representing the remote location of the source of an import path. Remote
// repositories can be bare import paths, or urls including a checkout scheme.
func deduceRemoteRepo(path string) (rr *remoteRepo, err error) {
	rr = &remoteRepo{}
	if m := scpSyntaxRe.FindStringSubmatch(path); m != nil {
		// Match SCP-like syntax and convert it to a URL.
		// Eg, "git@github.com:user/repo" becomes
		// "ssh://git@github.com/user/repo".
		rr.CloneURL = &url.URL{
			Scheme: "ssh",
			User:   url.User(m[1]),
			Host:   m[2],
			Path:   "/" + m[3],
			// TODO(sdboyer) This is what stdlib sets; grok why better
			//RawPath: m[3],
		}
	} else {
		rr.CloneURL, err = url.Parse(path)
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid import path", path)
		}
	}

	if rr.CloneURL.Host != "" {
		path = rr.CloneURL.Host + "/" + strings.TrimPrefix(rr.CloneURL.Path, "/")
	} else {
		path = rr.CloneURL.Path
	}

	if !pathvld.MatchString(path) {
		return nil, fmt.Errorf("%q is not a valid import path", path)
	}

	if rr.CloneURL.Scheme != "" {
		rr.Schemes = []string{rr.CloneURL.Scheme}
	}

	// TODO(sdboyer) instead of a switch, encode base domain in radix tree and pick
	// detector from there; if failure, then fall back on metadata work

	// No luck so far. maybe it's one of them vanity imports?
	// We have to get a little fancier for the metadata lookup - wrap a future
	// around a future
	var importroot, vcs string
	// We have a real URL. Set the other values and return.
	rr.Base = importroot
	rr.RelPkg = strings.TrimPrefix(path[len(importroot):], "/")

	rr.VCS = []string{vcs}
	if rr.CloneURL.Scheme != "" {
		rr.Schemes = []string{rr.CloneURL.Scheme}
	}

	return rr, nil
}

// fetchMetadata fetchs the remote metadata for path.
func fetchMetadata(path string) (rc io.ReadCloser, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("unable to determine remote metadata protocol: %s", err)
		}
	}()

	// try https first
	rc, err = doFetchMetadata("https", path)
	if err == nil {
		return
	}

	rc, err = doFetchMetadata("http", path)
	return
}

func doFetchMetadata(scheme, path string) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s://%s?go-get=1", scheme, path)
	switch scheme {
	case "https", "http":
		resp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("failed to access url %q", url)
		}
		return resp.Body, nil
	default:
		return nil, fmt.Errorf("unknown remote protocol scheme: %q", scheme)
	}
}

// parseMetadata fetches and decodes remote metadata for path.
func parseMetadata(path string) (string, string, string, error) {
	rc, err := fetchMetadata(path)
	if err != nil {
		return "", "", "", err
	}
	defer rc.Close()

	imports, err := parseMetaGoImports(rc)
	if err != nil {
		return "", "", "", err
	}
	match := -1
	for i, im := range imports {
		if !strings.HasPrefix(path, im.Prefix) {
			continue
		}
		if match != -1 {
			return "", "", "", fmt.Errorf("multiple meta tags match import path %q", path)
		}
		match = i
	}
	if match == -1 {
		return "", "", "", fmt.Errorf("go-import metadata not found")
	}
	return imports[match].Prefix, imports[match].VCS, imports[match].RepoRoot, nil
}

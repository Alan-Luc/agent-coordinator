package tmux

import (
	"regexp"
	"strings"
)

var safeToken = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

// Config returns immutable settings for one private coordinator server.
func TransparentConfig(owner, job string) string {
	if !safeToken.MatchString(owner) || !safeToken.MatchString(job) {
		return ""
	}
	return strings.Join([]string{
		"set-option -g @ac_owner " + owner,
		"set-option -g @ac_job " + job,
		"set-option -g @ac_protocol 2",
		"set-option -g prefix None",
		"set-option -g prefix2 None",
		"set-option -g extended-keys on",
		"set-option -g extended-keys-format csi-u",
		"set-option -g assume-paste-time 0",
		"set-option -g remain-on-exit on",
		"set-option -g exit-unattached off",
		"set-option -g exit-empty on",
		"set-option -g window-size latest",
		"set-option -g status off",
		"unbind-key -a -T root",
		"unbind-key -a -T prefix",
	}, "\n") + "\n"
}

func Config(owner, id, name string) string {
	if !safeToken.MatchString(owner) || !safeToken.MatchString(id) || !safeToken.MatchString(name) {
		return ""
	}
	return strings.Join([]string{
		"set-option -g @ac_owner " + owner,
		"set-option -g @ac_job " + id,
		"set-option -g @ac_protocol 1",
		"set-option -g prefix None",
		"set-option -g prefix2 None",
		"set-option -g extended-keys on",
		"set-option -g extended-keys-format csi-u",
		"set-option -g assume-paste-time 0",
		"set-option -g remain-on-exit on",
		"set-option -g exit-unattached off",
		"set-option -g exit-empty on",
		"set-option -g window-size latest",
		"set-option -g status on",
		"unbind-key -a -T root",
		"unbind-key -a -T prefix",
		"bind-key -T root F12 detach-client",
		"set-option -g status-left '#{?client_readonly,Observe,Enter} " + name + " (#{session_attached} clients)'",
		"set-option -g status-right 'F12 Return'",
	}, "\n") + "\n"
}

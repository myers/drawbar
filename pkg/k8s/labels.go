package k8s

// SanitizeLabelValue makes a string safe for k8s label values. K8s requires
// label values to match (([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])? — i.e.
// they must start and end with an alphanumeric (or be empty), be ≤63 chars,
// and only contain [-A-Za-z0-9_.]. We map invalid bytes to '-', trim leading
// and trailing non-alphanumerics, then truncate (trimming again if the cut
// landed on a non-alphanumeric).
//
// An all-non-alphanumeric input returns the empty string, which is also
// valid as a k8s label value. Callers that want to *omit* the label in
// that case should check for "" explicitly.
func SanitizeLabelValue(s string) string {
	b := make([]byte, 0, len(s))
	for _, c := range []byte(s) {
		if isLabelChar(c) {
			b = append(b, c)
		} else {
			b = append(b, '-')
		}
	}
	b = trimNonAlnum(b)
	if len(b) > 63 {
		b = trimNonAlnum(b[:63])
	}
	return string(b)
}

func isLabelChar(c byte) bool {
	return isAlnum(c) || c == '-' || c == '_' || c == '.'
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func trimNonAlnum(b []byte) []byte {
	for len(b) > 0 && !isAlnum(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && !isAlnum(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

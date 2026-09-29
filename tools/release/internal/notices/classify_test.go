package notices

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/tools/release/internal/licenses"
)

// apacheLicense is the Apache License 2.0 with its appendix, as projects
// ship it.
//
//go:embed testdata/apache-2.0.txt
var apacheLicense string

// goPatentGrant is the PATENTS file of the Go project.
//
//go:embed testdata/go-patents.txt
var goPatentGrant string

const mitLicense = `The MIT License (MIT)

Copyright (c) 2014 Example Author

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

const goLicense = `Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
`

const bsd2License = `BSD 2-Clause License

Copyright (c) 2020, Example Author
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
`

const iscLicense = `ISC License

Copyright (c) 2020 Example Author

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
`

// reflow rewraps text at a different width and indents it, as license
// files of different projects do with the same terms.
func reflow(text string, width int, indent string) string {
	var (
		b    strings.Builder
		line string
	)

	for paragraph := range strings.SplitSeq(text, "\n\n") {
		for word := range strings.FieldsSeq(paragraph) {
			if line != "" && len(line)+1+len(word) > width {
				b.WriteString(indent + line + "\n")
				line = ""
			}

			if line != "" {
				line += " "
			}

			line += word
		}

		b.WriteString(indent + line + "\n\n")
		line = ""
	}

	return b.String()
}

// after returns text from the first occurrence of marker on.
func after(t *testing.T, text, marker string) string {
	t.Helper()

	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("no %q in the text", marker)
	}

	return text[i:]
}

// replace replaces the single occurrence of old in text.
func replace(t *testing.T, text, old, replacement string) string {
	t.Helper()

	if strings.Count(text, old) != 1 {
		t.Fatalf("%q does not occur exactly once in the text", old)
	}

	return strings.Replace(text, old, replacement, 1)
}

func TestClassifyRecognizesEachLicense(t *testing.T) {
	cases := map[string]struct{ text, want string }{
		"MIT":                              {text: mitLicense, want: "MIT"},
		"MIT without title":                {text: after(t, mitLicense, "Copyright"), want: "MIT"},
		"MIT titled with the module":       {text: replace(t, mitLicense, "Copyright (c) 2014", "go-toml v2\nCopyright (c) 2021 - 2023"), want: "MIT"},
		"Apache-2.0":                       {text: apacheLicense, want: "Apache-2.0"},
		"Apache-2.0 reflowed and indented": {text: reflow(apacheLicense, 66, "   "), want: "Apache-2.0"},
		"Apache-2.0 with the appendix filled in and https": {
			text: strings.ReplaceAll(replace(t, apacheLicense, "Copyright [yyyy] [name of copyright owner]",
				"Copyright 2019, 2020 Example Contributors\nCopyright 2016 Example, Inc."), "http://", "https://"),
			want: "Apache-2.0",
		},
		"BSD-3-Clause of the Go project": {text: goLicense, want: "BSD-3-Clause"},
		"BSD-3-Clause with two copyrights": {
			text: replace(t, goLicense, "Copyright 2009 The Go Authors.",
				"Copyright (c) 2012 Alex Example. All rights reserved.\nCopyright (c) 2012 The Go Authors. All rights reserved."),
			want: "BSD-3-Clause",
		},
		"BSD-2-Clause": {text: bsd2License, want: "BSD-2-Clause"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := classify(tc.text)
			if err != nil || got != tc.want {
				t.Errorf("classify = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestClassifyRefusesUnknownLicenses(t *testing.T) {
	disclaimer := after(t, mitLicense, "THE SOFTWARE IS PROVIDED")

	cases := map[string]struct{ text, want string }{
		"ISC, a license outside the accepted set": {text: iscLicense, want: "licensecheck finds ISC;"},
		"MIT with an added clause": {
			text: replace(t, mitLicense, "copies or substantial portions of the Software.",
				"copies or substantial portions of the Software.\n\nThe Software shall be used for Good, not Evil."),
		},
		"MIT with a restriction after the terms": {
			text: mitLicense + "\nThe Software may not be used in any product that is sold in a member state of the European Union.\n",
			want: "MIT followed by other text",
		},
		"MIT without its warranty disclaimer": {text: strings.TrimSuffix(mitLicense, disclaimer)},
		"BSD-3-Clause with an advertising clause": {
			text: replace(t, goLicense, "this software without specific prior written permission.",
				"this software without specific prior written permission.\n   * All advertising materials mentioning features or use of this\n"+
					"software must display an acknowledgement."),
		},
		"Apache-2.0 with an exception": {
			text: apacheLicense + "\n\n---- LLVM Exceptions to the Apache 2.0 License ----\n\n" +
				"As an exception, if you use this Software to compile your source code and portions of this Software are embedded into " +
				"the binary product as a result, you may redistribute such product without providing attribution as would otherwise be " +
				"required by Sections 4(a), 4(b) and 4(d) of the License.\n",
			want: "Apache-2.0 followed by other text",
		},
		"Apache-2.0 after a restriction": {
			text: "This software may not be used for any military purpose whatsoever by anyone.\n\n" + apacheLicense,
			want: "Apache-2.0 preceded by more than 8 words",
		},
		"MIT and Apache-2.0 in one file": {text: mitLicense + "\n" + apacheLicense, want: "MIT + Apache-2.0, not a single license"},
		"Apache-2.0 and MIT in one file": {text: apacheLicense + "\n" + mitLicense, want: "Apache-2.0 + MIT"},
		"only the URL of a license":      {text: "Licensed under https://www.apache.org/licenses/LICENSE-2.0\n", want: "(URL)"},
		"unknown text":                   {text: "All rights reserved. Do not copy.\n", want: "no license"},
		"empty":                          {text: "\n\n", want: "the text is empty"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := classify(tc.text)

			switch {
			case err == nil:
				t.Errorf("classify = %q, want an error", got)
			case !strings.Contains(err.Error(), "the license text is not recognized") || !strings.Contains(err.Error(), tc.want):
				t.Errorf("classify: %v; want the message to contain %q", err, tc.want)
			}
		})
	}
}

func TestComponentLicenseReadsTheFileRoles(t *testing.T) {
	file := func(name, text string) licenses.LicenseFile { return licenses.LicenseFile{Name: name, Text: text} }

	cases := map[string]struct {
		files []licenses.LicenseFile
		want  string
		err   string
	}{
		"license, notice and documentation license": {
			files: []licenses.LicenseFile{file("LICENSE", apacheLicense), file("LICENSE.docs", "Creative Commons"), file("NOTICE", "Example")},
			want:  "Apache-2.0",
		},
		"license and the Go patent grant": {
			files: []licenses.LicenseFile{file("LICENSE", goLicense), file("PATENTS", reflow(goPatentGrant, 60, ""))},
			want:  "BSD-3-Clause",
		},
		"license.txt": {files: []licenses.LicenseFile{file("License.txt", mitLicense)}, want: "MIT"},
		"another patent grant": {
			files: []licenses.LicenseFile{file("LICENSE", goLicense), file("PATENTS", "Example grants a patent license.")},
			err:   "PATENTS is not a patent grant in the form of the Go project's PATENTS file",
		},
		"patent grant with an added condition": {
			files: []licenses.LicenseFile{
				file("LICENSE", goLicense),
				file("PATENTS", goPatentGrant+"\nThis grant applies only to users who have registered their copy of Go with Google Inc.\n"),
			},
			err: "PATENTS is not a patent grant",
		},
		"dual license files": {
			files: []licenses.LicenseFile{file("LICENSE-APACHE", apacheLicense), file("LICENSE-MIT", mitLicense)},
			err:   "LICENSE-APACHE: cannot tell which code",
		},
		"two license files": {
			files: []licenses.LicenseFile{file("COPYING", mitLicense), file("LICENSE", mitLicense)},
			err:   "found 2 license files",
		},
		"notice only":          {files: []licenses.LicenseFile{file("NOTICE", "Example")}, err: "found 0 license files"},
		"unrecognized license": {files: []licenses.LicenseFile{file("LICENSE", iscLicense)}, err: "LICENSE: the license text is not recognized"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := componentLicense(tc.files)

			switch {
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Errorf("componentLicense = %q, %v; want an error containing %q", got, err, tc.err)
			case tc.err == "" && (err != nil || got != tc.want):
				t.Errorf("componentLicense = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

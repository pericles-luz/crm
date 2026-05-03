// Out-of-scope fixture: a package OUTSIDE the protected substring is
// allowed to use net/http. The analyzer must NOT report.
package other

import "net/http"

func DoOtherThing() *http.Client { return http.DefaultClient }

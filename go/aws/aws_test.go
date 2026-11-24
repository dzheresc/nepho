package aws

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

const groupsXML = `<DescribeSecurityGroupsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><securityGroupInfo>
<item><groupId>sg-m</groupId><tagSet><item><key>nepho-managed</key><value>true</value></item>
<item><key>nepho-id</key><value>m-old</value></item><item><key>nepho-expires</key><value>1</value></item></tagSet></item>
<item><groupId>sg-s</groupId><tagSet><item><key>nepho-managed</key><value>true</value></item>
<item><key>nepho-id</key><value>s-old</value></item><item><key>nepho-expires</key><value>1</value></item>
<item><key>nepho-kind</key><value>postgres</value></item></tagSet></item>
</securityGroupInfo></DescribeSecurityGroupsResponse>`

// Reap must remove an expired machine's group and leave a service's group alone.
func TestReapSkipsServices(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q, _ := url.ParseQuery(string(body))
		out := "<" + q.Get("Action") + "Response/>"
		switch q.Get("Action") {
		case "DescribeSecurityGroups":
			out = groupsXML
			if q.Get("Filter.1.Name") == "tag:nepho-id" { // only the group of that id
				items := strings.Split(groupsXML, "<item><groupId>")
				for _, item := range items[1:] {
					if strings.Contains(item, q.Get("Filter.1.Value.1")) {
						out = items[0] + "<item><groupId>" + strings.TrimSuffix(strings.TrimSpace(item),
							"</securityGroupInfo></DescribeSecurityGroupsResponse>") +
							"</securityGroupInfo></DescribeSecurityGroupsResponse>"
					}
				}
			}
		case "DeleteSecurityGroup":
			deleted = append(deleted, q.Get("GroupId"))
		}
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, out)
	}))
	defer srv.Close()
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ctx := context.Background()
	p, err := New(ctx, Options{Region: "us-east-1", KeyPath: filepath.Join(t.TempDir(), "id_rsa")})
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Reap(ctx)
	if err != nil || n != 1 || len(deleted) != 1 || deleted[0] != "sg-m" {
		t.Fatalf("n=%d deleted=%v err=%v", n, deleted, err)
	}
}

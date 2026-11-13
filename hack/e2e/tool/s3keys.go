package main

import (
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// s3keys counts the keys under a prefix of one bucket on one S3 endpoint,
// and with --delete removes them: how the harness counts a shard's objects
// and empties a bucket on any S3 store (Garage has no admin call that
// empties one). Credentials are AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
//
//	c4e2e s3keys --endpoint http://127.0.0.1:31900 --bucket B [--prefix c4/] [--delete]
func s3keys(args []string) error {
	fs := flag.NewFlagSet("s3keys", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "the S3 endpoint, such as http://127.0.0.1:31900")
	bucket := fs.String("bucket", "", "bucket")
	prefix := fs.String("prefix", "c4/", "key prefix")
	region := fs.String("region", "us-east-1", "region")
	del := fs.Bool("delete", false, "delete every key listed")
	fs.Parse(args)
	if *endpoint == "" || *bucket == "" {
		return fmt.Errorf("s3keys needs --endpoint and --bucket")
	}
	c := s3client{
		endpoint: strings.TrimSuffix(*endpoint, "/"), bucket: *bucket, region: *region,
		creds: storeurl.Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY")},
		http:  &http.Client{Timeout: time.Minute},
	}
	keys, err := c.list(context.Background(), *prefix)
	if err != nil {
		return err
	}
	if *del {
		for _, k := range keys {
			if err := c.delete(context.Background(), k); err != nil {
				return err
			}
		}
	}
	fmt.Println(len(keys))
	return nil
}

type s3client struct {
	endpoint, bucket, region string
	creds                    storeurl.Credentials
	http                     *http.Client
}

func (c s3client) do(ctx context.Context, method, path string, q url.Values) (*http.Response, error) {
	u := c.endpoint + "/" + c.bucket + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	storeurl.SignRequest(req, c.creds, c.region)
	return c.http.Do(req)
}

// list is every key under prefix, page by page (ListObjectsV2).
func (c s3client) list(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list %s/%s: %s: %s", c.endpoint, c.bucket, resp.Status, strings.TrimSpace(string(body)))
		}
		var page struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("list %s/%s: %w", c.endpoint, c.bucket, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, o.Key)
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return keys, nil
		}
		token = page.NextContinuationToken
	}
}

func (c s3client) delete(ctx context.Context, key string) error {
	var esc []string
	for _, seg := range strings.Split(key, "/") {
		esc = append(esc, url.PathEscape(seg))
	}
	resp, err := c.do(ctx, http.MethodDelete, "/"+strings.Join(esc, "/"), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete %s: %s: %s", key, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

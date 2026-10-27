# Resend Go SDK

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)
![Build](https://github.com/resend/resend-go/actions/workflows/go.yml/badge.svg)
![Release](https://img.shields.io/github/release/resend/resend-go.svg?style=flat-square)
[![Go Reference](https://pkg.go.dev/badge/github.com/resend/resend-go/v4.svg)](https://pkg.go.dev/github.com/resend/resend-go/v4)
---

## Installation

To install the Go SDK, simply execute the following command on a terminal:

```
go get github.com/resend/resend-go/v4
```

## Setup

First, you need to get an API key, which is available in the [Resend Dashboard](https://resend.com).

## Example

```go
package main

import (
    "fmt"
    "github.com/resend/resend-go/v4"
)

func main() {
    apiKey := "re_123"

    client := resend.NewClient(apiKey)

    params := &resend.SendEmailRequest{
        To:      []string{"to@example", "you@example.com"},
        From:    "me@exemple.io",
        Text:    "hello world",
        Subject: "Hello from Golang",
        Cc:      []string{"cc@example.com"},
        Bcc:     []string{"cc@example.com"},
        ReplyTo: "replyto@example.com",
    }

    sent, err := client.Emails.Send(params)
    if err != nil {
        panic(err)
    }
    fmt.Println(sent.Id)
}

```

## Retries

Retries are off by default. To enable them, use `RetryTransport`:

```go
client := resend.NewCustomClient(&http.Client{
    Timeout:   2 * time.Minute, // covers all attempts
    Transport: resend.NewRetryTransport(nil, resend.RetryPolicy{}),
}, apiKey)
```

- A `429` is retried after its `Retry-After` delay.
- A `500`, `502`, `503`, `504` or network error is retried with exponential backoff and jitter, but only for `GET`, `HEAD`, `OPTIONS`, `PUT` and `DELETE`, or for requests with an idempotency key. A send without an idempotency key is never retried after these errors, so an email is never sent twice.
- Other errors are returned right away.

The defaults are 3 retries, a 500ms base delay and a 30s maximum delay.

You can view all the examples in the [examples folder](https://github.com/resend/resend-go/tree/main/examples)

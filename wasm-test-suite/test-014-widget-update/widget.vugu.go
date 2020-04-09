package main

import (
    "github.com/vugu/vugu"
)

type Widget struct {
    Hidden  bool   `vugu:"data"`
}

func (c *Widget) DeleteHandler(event *vugu.DOMEvent) {
    c.Hidden = true
}
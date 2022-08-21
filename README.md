# REMCO KUBERNETES PLUGIN

## Purpose

This plugin for [remco](https://github.com/HeavyHorst/remco) is made to retrieve resources from the kubernetes API.
It is mainly based on the confd backend created by @jfrabaut.
For now it can retrieve services, endpoints and statefulsets.

## How to install it

1. install remco
2. clone the repo
3. go get
4. go build
5. cp remco-plugin-kubernetes /etc/remco/plugins/kubernetes

## How to use it

You can find configuration and template examples in examples directory

Configuration parameters are :
* **host** : url of kubernetes API, if not set plugin will consider running inside kubernetes and will connect to the local API server
* **username** : if using standard auth, username to login
* **password** : if using standard auth, password used to login
* **token** : bearer token, if not using username/password to authenticate
* **ca** : file containing the CA
* **key** : file containing the key, if required
* **cert** : file containing the cert, if required
* **log_level** : log level of the plugin
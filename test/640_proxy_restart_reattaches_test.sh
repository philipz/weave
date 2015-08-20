#! /bin/bash

. ./config.sh

NAME=seetwo.weave.local

start_suite "Proxy restart reattaches networking to containers"

weave_on $HOST1 launch
proxy_start_container          $HOST1 --name=c2 -h $NAME
proxy_start_container_with_dns $HOST1 --name=c1

C2=$(container_ip $HOST1 c2)
proxy docker_on $HOST1 restart --time=1 c2
assert_raises "proxy exec_on $HOST1 c2 $CHECK_ETHWE_UP"
assert_dns_record $HOST1 c1 $NAME $C2

end_suite

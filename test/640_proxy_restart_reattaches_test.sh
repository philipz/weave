#! /bin/bash

. ./config.sh

NAME=seetwo.weave.local

N=50
# Create and remove a lot of containers in a small subnet; the failure
# mode is that this takes a long time as it has to wait for the old
# ones to time out, so we run this function inside 'timeout'
run_many() {
    for i in $(seq $N); do
        proxy docker_on $HOST1 run -e WEAVE_CIDR=net:10.32.4.0/28 --rm -t $SMALL_IMAGE /bin/true
    done
}

start_suite "Proxy restart reattaches networking to containers"

weave_on $HOST1 launch
proxy_start_container          $HOST1 --name=c2 -h $NAME
proxy_start_container_with_dns $HOST1 --name=c1

C2=$(container_ip $HOST1 c2)
proxy docker_on $HOST1 restart --time=1 c2
assert_raises "proxy exec_on $HOST1 c2 $CHECK_ETHWE_UP"
assert_dns_record $HOST1 c1 $NAME $C2

assert_raises "timeout $N cat <( run_many )"

end_suite

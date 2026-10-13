# MikroTik RouterOS v7 - setup for um-status-gateway
# Edit only the SETTINGS block, then import this file in the RouterOS terminal.
# The script reuses matching objects and tagged rules instead of adding duplicates.
# It does not remove or overwrite unrelated router configuration.

# ------------------------------- SETTINGS ----------------------------------
:local containerName "umgw"
:local imageName "ghcr.io/hashed-albaham/mikrotik-um-gateway:latest"
:local rootDir "disk1/images/umgw"
:local tempDir "disk1/tmp"

# Private container network. Change this block to choose a different IP/subnet.
:local bridgeName "containers"
:local vethName "veth-umgw"
:local networkCidr "172.17.0.0/24"
:local containerCidr "172.17.0.2/24"
:local containerIP "172.17.0.2"
:local gatewayCidr "172.17.0.1/24"
:local gatewayIP "172.17.0.1"

# MikroTik User Manager portal, reachable from the container.
:local umProtocol "http"
:local umHost "192.168.216.1"
:local umPort "12008"

# Local service settings.
:local servicePort "8080"
:local logoutDelay "60"
:local requestTimeout "8"

# Set to true only if this container should be started after router reboot.
:local startOnBoot true
# ---------------------------------------------------------------------------

:local envList "ENV_UMGW"
:local natComment "UM status gateway: container network NAT"
:local gardenComment "UM status gateway: hotspot access"

# Container support must already be enabled in RouterOS Device Mode.

# Bridge and router-side gateway address.
:if ([:len [/interface/bridge/find where name=$bridgeName]] = 0) do={
    /interface/bridge/add name=$bridgeName comment="UM status gateway network"
}
:local bridgeIds [/ip/address/find where interface=$bridgeName]
:if ([:len $bridgeIds] = 0) do={
    /ip/address/add address=$gatewayCidr interface=$bridgeName comment="UM status gateway"
} else={
    /ip/address/set [:pick $bridgeIds 0] address=$gatewayCidr
}

# VETH address and its bridge port.
:if ([:len [/interface/veth/find where name=$vethName]] = 0) do={
    /interface/veth/add name=$vethName address=$containerCidr gateway=$gatewayIP comment="UM status gateway"
} else={
    /interface/veth/set [find where name=$vethName] address=$containerCidr gateway=$gatewayIP
}
:if ([:len [/interface/bridge/port/find where bridge=$bridgeName and interface=$vethName]] = 0) do={
    /interface/bridge/port/add bridge=$bridgeName interface=$vethName
}

# Outbound access from the private container subnet. Use a dedicated comment so
# rerunning this script only updates this rule, not other NAT/firewall rules.
:if ([:len [/ip/firewall/nat/find where comment=$natComment]] = 0) do={
    /ip/firewall/nat/add chain=srcnat action=masquerade src-address=$networkCidr comment=$natComment
} else={
    /ip/firewall/nat/set [find where comment=$natComment] src-address=$networkCidr
}

# Environment list and settings consumed by the gateway.
:if ([:len [/container/envs/find where list=$envList and key="UM_PROTO"]] = 0) do={
    /container/envs/add list=$envList key="UM_PROTO" value=$umProtocol
} else={
    /container/envs/set [find where list=$envList and key="UM_PROTO"] value=$umProtocol
}
:if ([:len [/container/envs/find where list=$envList and key="UM_HOST"]] = 0) do={
    /container/envs/add list=$envList key="UM_HOST" value=$umHost
} else={
    /container/envs/set [find where list=$envList and key="UM_HOST"] value=$umHost
}
:if ([:len [/container/envs/find where list=$envList and key="UM_PORT"]] = 0) do={
    /container/envs/add list=$envList key="UM_PORT" value=$umPort
} else={
    /container/envs/set [find where list=$envList and key="UM_PORT"] value=$umPort
}
:if ([:len [/container/envs/find where list=$envList and key="LISTEN_ADDR"]] = 0) do={
    /container/envs/add list=$envList key="LISTEN_ADDR" value=(":" . $servicePort)
} else={
    /container/envs/set [find where list=$envList and key="LISTEN_ADDR"] value=(":" . $servicePort)
}
:if ([:len [/container/envs/find where list=$envList and key="LOGOUT_DELAY_SECONDS"]] = 0) do={
    /container/envs/add list=$envList key="LOGOUT_DELAY_SECONDS" value=$logoutDelay
} else={
    /container/envs/set [find where list=$envList and key="LOGOUT_DELAY_SECONDS"] value=$logoutDelay
}
:if ([:len [/container/envs/find where list=$envList and key="REQUEST_TIMEOUT_SECONDS"]] = 0) do={
    /container/envs/add list=$envList key="REQUEST_TIMEOUT_SECONDS" value=$requestTimeout
} else={
    /container/envs/set [find where list=$envList and key="REQUEST_TIMEOUT_SECONDS"] value=$requestTimeout
}

# Registry settings and container image.
/container/config/set registry-url="https://ghcr.io" tmpdir=$tempDir memory-high=64M
:local containerIds [/container/find where name=$containerName]
:if ([:len $containerIds] = 0) do={
    /container/add remote-image=$imageName interface=$vethName root-dir=$rootDir envlist=$envList name=$containerName start-on-boot=$startOnBoot logging=yes
}

# Walled-garden exception lets unauthenticated hotspot clients reach the API.
:if ([:len [/ip/hotspot/walled-garden/ip/find where comment=$gardenComment]] = 0) do={
    /ip/hotspot/walled-garden/ip/add action=accept protocol=tcp dst-address=$containerIP dst-port=$servicePort comment=$gardenComment
} else={
    /ip/hotspot/walled-garden/ip/set [find where comment=$gardenComment] dst-address=$containerIP dst-port=$servicePort
}

:put "UM status gateway configuration is ready."
:put ("Container IP: " . $containerIP . ":" . $servicePort)
:put ("User Manager portal: " . $umProtocol . "://" . $umHost . ":" . $umPort)
:put "Check /container/print and /container/log/print; start the container with /container/start umgw if it is not running."
:put "If the GHCR package is private, configure registry credentials separately before pulling the image."

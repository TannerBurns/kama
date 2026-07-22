.metadata.name == $name and
.metadata.uid == $uid and
.spec.type == "ClusterIP" and
(.spec.ports | length) == 1 and
.spec.ports[0].name == "http" and
.spec.ports[0].protocol == "TCP" and
.spec.ports[0].port == 8080 and
.spec.ports[0].targetPort == "http" and
([.spec.ports[] | select(.port == 8081 or .targetPort == 8081)] | length) == 0

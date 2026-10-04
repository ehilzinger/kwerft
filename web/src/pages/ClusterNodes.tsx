import { getRouteApi } from "@tanstack/react-router";
import { ClusterLayout } from "./Clusters";

const route = getRouteApi("/authed/clusters/$name/nodes");

// A cluster › Nodes: node pools, servers, join and remove (W2, Phase 5).
export function ClusterNodes() {
  const { name } = route.useParams();
  return <ClusterLayout cluster={name} current="nodes"><div className="empty"><h2>Coming in Phase 5</h2></div></ClusterLayout>;
}

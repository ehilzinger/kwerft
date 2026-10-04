import { getRouteApi } from "@tanstack/react-router";
import { ClusterLayout } from "./Clusters";

const route = getRouteApi("/authed/clusters/$name");

// A cluster › Overview: health, agent, versions (W3, Phase 5).
export function ClusterOverview() {
  const { name } = route.useParams();
  return <ClusterLayout cluster={name} current="overview"><div className="empty"><h2>Coming in Phase 5</h2></div></ClusterLayout>;
}

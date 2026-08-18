import { CompassIcon } from "lucide-react"

import { LinkButton } from "@/components/link-button"
import { Empty } from "@/components/empty"
import { Card, CardContent } from "@/components/ui/card"

export default function NotFoundPage() {
  return (
    <Card>
      <CardContent>
        <Empty
          icon={<CompassIcon />}
          title="Nothing here"
          description="That page does not exist. It may have been renamed, or the link may be from an older version."
        >
          <LinkButton to="/" size="sm">
            Back to the overview
          </LinkButton>
        </Empty>
      </CardContent>
    </Card>
  )
}

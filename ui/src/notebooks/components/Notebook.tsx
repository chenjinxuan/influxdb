// Libraries
import React, {FC} from 'react'

// Components
import {Page} from '@influxdata/clockface'
import {NotebookProvider} from 'src/notebooks/context/notebook'
import {ScrollProvider} from 'src/notebooks/context/scroll'
import Header from 'src/notebooks/components/header'
import PipeList from 'src/notebooks/components/PipeList'
import MiniMap from 'src/notebooks/components/minimap/MiniMap'
import GetResources from 'src/resources/components/GetResources'

// Types
import {ResourceType} from 'src/types'

// NOTE: uncommon, but using this to scope the project
// within the page and not bleed it's dependancies outside
// of the feature flag
import 'src/notebooks/style.scss'

const NotebookPage: FC = () => {
  return (
    <NotebookProvider>
      <ScrollProvider>
        <GetResources resources={[ResourceType.Buckets]}>
          <Page titleTag="Flows">
            <Header />
            <Page.Contents
              fullWidth={true}
              scrollable={false}
              className="notebook-page"
            >
              <div className="notebook">
                <MiniMap />
                <PipeList />
              </div>
            </Page.Contents>
          </Page>
        </GetResources>
      </ScrollProvider>
    </NotebookProvider>
  )
}

export default NotebookPage
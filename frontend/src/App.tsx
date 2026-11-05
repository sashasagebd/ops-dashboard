import { AppPage } from './AppPage'
import { AppsPage } from './Apps'
import { ContainersPage } from './Containers'
import { OverviewPage } from './Overview'
import { useRoute, type Route } from './route'
import { Shell } from './Shell'
import { PageHeader } from './ui'

function App() {
  const route = useRoute()
  return (
    <Shell route={route}>
      <Page route={route} />
    </Shell>
  )
}

function Page({ route }: { route: Route }) {
  switch (route.page) {
    case 'overview':
      return <OverviewPage />
    case 'containers':
      return <ContainersPage />
    case 'apps':
      return <AppsPage />
    case 'app':
      // Keyed by id, so moving to another app starts from a clean state;
      // not by tab, so switching tabs keeps what the page holds.
      return <AppPage key={route.id} id={route.id} tab={route.tab} />
    case 'not-found':
      return (
        <PageHeader
          title="No such page"
          subtitle={
            <>
              No such page. <a href="#/">Back to the overview</a>
            </>
          }
        />
      )
  }
}

export default App

// The Worker's entry: workerd allows only handlers as its exports, so the
// logic lives in delivery.ts.
import { createWorker } from './delivery.ts'

export default createWorker()

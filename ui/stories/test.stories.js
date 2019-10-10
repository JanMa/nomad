
import hbs from 'htmlbars-inline-precompile';
import { storiesOf } from '@storybook/ember';
import notes from './test.md';

storiesOf('Test/', module)
  .addParameters({ options: { showPanel: false } })
  .add(
    `Test`,
    () => ({
      template: hbs`
      <h2>a story</h2>
      <p>baby’s first story 🎉 {{copy-button clipboardText="baby" tagName="span"}}</p>
    `,
      context: {
        close: () => {
          console.log('closing!');
        },
        message: 'Hello!',
      },
    }),
    { notes }
  );
